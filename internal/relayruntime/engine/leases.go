// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"context"
	"sync"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

// Access-lease synchronisation for one direct-mode session (ADR-006).
//
// The relay long-polls the broker for the full set of per-client credentials
// it must accept, reconciles its Xray users to that set, and acknowledges the
// set's revision only once every Xray call succeeded — the broker releases a
// lease to a client only after the relay that must accept it has acked a
// revision covering it. The syncer runs in its own goroutine because the poll
// blocks for up to the broker's hold; the heartbeat loop never waits on it.
// Lease users are labelled relay.LeaseEmailPrefix+id, disjoint from the
// rotating shared credentials' "cred-" labels, so credentialRotation.retire
// and the reconcile below never touch each other's users.

var (
	// leaseSyncMinBackoff and leaseSyncMaxBackoff bound the retry delay after a
	// failed poll, reconcile or ack. Variables so tests can shorten them.
	leaseSyncMinBackoff = time.Second
	leaseSyncMaxBackoff = 30 * time.Second
	// leaseExpiryInterval is how often locally expired leases are removed
	// without broker contact — the broker-outage behaviour.
	leaseExpiryInterval = 30 * time.Second
)

// leaseSession is the mutex-guarded (relay ID, lease token) the syncer
// authenticates with. sendHeartbeat updates it after a re-registration, and
// the syncer re-reads it on every cycle: a new token is a new broker-side
// session whose applied revision the broker reset, so the syncer starts that
// session with a full fetch and ack whether or not it ever saw a 404. The
// generation counter is how it notices the change; changed is closed (and
// replaced) on every change, so a poll or wait that holds the old token ends
// as soon as a re-registration mints a new one.
type leaseSession struct {
	mu         sync.Mutex
	id         string
	token      string
	generation uint64
	changed    chan struct{}
}

func (s *leaseSession) set(id, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.id && token == s.token {
		return
	}
	s.id, s.token = id, token
	s.generation++
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// get returns the current session and a channel closed when it changes.
func (s *leaseSession) get() (id, token string, generation uint64, changed <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.id, s.token, s.generation, s.changed
}

// superseded reports whether the session moved past generation.
func (s *leaseSession) superseded(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation != generation
}

// installedLease is one lease the relay's Xray currently accepts.
type installedLease struct {
	credential string
	// expiresAt is the local wall-clock expiry: now + (expires_at − server_time)
	// at the time the lease was last seen, so a broker/relay clock offset
	// never shifts the window.
	expiresAt time.Time
}

// leaseSync drives one session's lease set.
type leaseSync struct {
	broker  *relayruntime.BrokerClient
	session *leaseSession
	users   relayruntime.XrayUserManager
	// heartbeatInterval is how long to wait after a 404 for the heartbeat to
	// re-register and refresh the session token.
	heartbeatInterval time.Duration
	logf              func(string, ...any)
	// onApplied runs after the first successful ack of a session (the point
	// at which the relay is fully online for lease holders).
	onApplied func()
	now       func() time.Time

	mu        sync.Mutex
	installed map[string]installedLease // by lease ID
	// applied is the revision of the set the relay has fully installed;
	// pendingAck is set from the moment a set is installed until the broker
	// has confirmed the ack for it, so a failed ack is retried before the
	// next poll no matter which path produced it.
	applied    int64
	pendingAck bool
	// full asks the next poll for the whole set with no hold: at session
	// start, after a re-registration, or after a reconcile that failed. A
	// full fetch is always acked, even at an unchanged revision — that is
	// how a new broker-side session learns the set is installed.
	full bool
	// generation is the leaseSession generation this state belongs to.
	generation uint64
	// announced records that onApplied has run.
	announced bool
	// active is whether the broker currently confirms the capability. While
	// inactive the syncer polls nothing and drains: it keeps retrying the
	// removal of every installed lease (and keeps the expiry sweep running)
	// until none is left, so a failed removal never strands a credential
	// without its expiry enforcement. activate wakes the loop on a change.
	active   bool
	activate chan struct{}
}

func newLeaseSync(broker *relayruntime.BrokerClient, session *leaseSession, users relayruntime.XrayUserManager, heartbeatInterval time.Duration, logf func(string, ...any)) *leaseSync {
	return &leaseSync{
		broker:            broker,
		session:           session,
		users:             users,
		heartbeatInterval: heartbeatInterval,
		logf:              logf,
		now:               time.Now,
		installed:         map[string]installedLease{},
		full:              true,
		activate:          make(chan struct{}, 1),
	}
}

// setActive turns synchronisation on (the broker echoed the capability) or
// off (it stopped). Turning it on starts the next cycle with a full fetch;
// turning it off switches the loop to draining.
func (l *leaseSync) setActive(active bool) {
	l.mu.Lock()
	changed := l.active != active
	l.active = active
	if active {
		l.full = true
		l.pendingAck = false
	}
	l.mu.Unlock()
	if changed {
		select {
		case l.activate <- struct{}{}:
		default:
		}
	}
}

// sessionMovedPast reports whether the session's current generation differs
// from the one the last cycle worked against.
func (l *leaseSync) sessionMovedPast(generation uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generation != generation
}

func (l *leaseSync) isActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// run is the session's lease worker. While active it polls, reconciles and
// acks; while inactive it drains — retrying the removal of every installed
// lease with backoff until none is left, then waiting for activation. When
// ctx ends it makes one best-effort removal pass (the session's xray is
// about to be stopped anyway). The expiry sweep runs on its own ticker for
// as long as the worker lives, so an unreachable broker (a poll blocked in
// backoff) or a failed removal never delays local expiry.
func (l *leaseSync) run(ctx context.Context) {
	expiry := time.NewTicker(leaseExpiryInterval)
	defer expiry.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-expiry.C:
				l.expireLocal(ctx)
			}
		}
	}()

	backoff := leaseSyncMinBackoff
	for {
		if ctx.Err() != nil {
			l.removeAll(ctx)
			return
		}
		var ok bool
		polling := l.isActive()
		if polling {
			ok = l.step(ctx)
		} else {
			ok = l.removeAll(ctx)
			if ok {
				// Drained: nothing to do until the broker confirms the
				// capability again.
				select {
				case <-ctx.Done():
					l.removeAll(ctx)
					return
				case <-l.activate:
				}
				backoff = leaseSyncMinBackoff
				continue
			}
		}
		if ok {
			backoff = leaseSyncMinBackoff
			continue
		}
		if ctx.Err() != nil {
			l.removeAll(ctx)
			return
		}
		// A new session (a re-registration) ends a polling failure's
		// backoff: the failure most likely belonged to the old token, and a
		// change that landed after the failed cycle began skips the wait
		// outright. A failed drain is local (Xray removals) — a session
		// change says nothing about it, and the generation it would compare
		// is only advanced by a poll cycle — so it keeps its backoff.
		var changed <-chan struct{}
		if polling {
			var generation uint64
			_, _, generation, changed = l.session.get()
			if l.sessionMovedPast(generation) {
				backoff = leaseSyncMinBackoff
				continue
			}
		}
		select {
		case <-ctx.Done():
			l.removeAll(ctx)
			return
		case <-l.activate:
		case <-changed:
			backoff = leaseSyncMinBackoff
			continue
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > leaseSyncMaxBackoff {
			backoff = leaseSyncMaxBackoff
		}
	}
}

// step performs one poll → reconcile → ack cycle and reports whether it
// completed without error. A pending ack is retried before polling again, so
// a lost ack never leaves the broker waiting on a set the relay already
// installed, and the relay's own online transition never depends on which
// attempt finally succeeded.
func (l *leaseSync) step(ctx context.Context) bool {
	id, token, generation, changed := l.session.get()
	if id == "" || token == "" {
		return false
	}
	// Every broker call of this cycle holds this session's token: a
	// re-registration that mints a new one cancels them (a long poll would
	// otherwise sit out its hold on a dead token), and the cycle reports
	// success so the loop starts over at once with the new session.
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-changed:
			cancel()
		case <-sessionCtx.Done():
		}
	}()
	ok := l.cycle(sessionCtx, id, token, generation, changed)
	if !ok && ctx.Err() == nil && l.session.superseded(generation) {
		return true
	}
	return ok
}

// cycle is one step against a fixed session (see step).
func (l *leaseSync) cycle(ctx context.Context, id, token string, generation uint64, changed <-chan struct{}) bool {
	l.mu.Lock()
	if generation != l.generation {
		// A new registration session: the broker reset its applied revision
		// when it minted this token, so start over with a full fetch and ack.
		// The installed users stay — the full set reconciles them.
		l.generation = generation
		l.full = true
		l.pendingAck = false
	}
	full, applied, pending := l.full, l.applied, l.pendingAck
	l.mu.Unlock()

	if pending && !full {
		if !l.ack(ctx, id, token, applied, changed) {
			return false
		}
	}

	after := int64(-1)
	if !full {
		after = applied
	}
	resp, err := l.broker.PollLeases(ctx, id, token, after)
	if err != nil {
		return l.handleBrokerError(ctx, "poll", err, changed)
	}
	now := l.now()
	if !l.reconcile(ctx, resp, now) {
		// Retry with a full, immediate poll rather than holding for a change
		// that may never come while a user is missing.
		l.mu.Lock()
		l.full = true
		l.mu.Unlock()
		return false
	}
	if resp.Revision < applied && !full {
		// A broker rolled back to an older state (restored backup); treat the
		// served set as authoritative and start counting from it.
		l.logf("lease revision went backwards (%d → %d); following the broker", applied, resp.Revision)
	}
	l.mu.Lock()
	revisionChanged := resp.Revision != l.applied
	l.applied = resp.Revision
	if full || revisionChanged || l.pendingAck {
		l.pendingAck = true
	}
	l.full = false
	pending = l.pendingAck
	l.mu.Unlock()
	if pending {
		return l.ack(ctx, id, token, resp.Revision, changed)
	}
	return true
}

// ack confirms revision to the broker and, on success, clears the pending
// ack and fires onApplied once per syncer. Every ack — first attempt or
// retry — goes through here so the online transition cannot be skipped.
func (l *leaseSync) ack(ctx context.Context, id, token string, revision int64, changed <-chan struct{}) bool {
	if err := l.broker.AckLeases(ctx, id, token, revision); err != nil {
		return l.handleBrokerError(ctx, "ack", err, changed)
	}
	l.mu.Lock()
	if l.applied == revision {
		l.pendingAck = false
	}
	announce := !l.announced && l.onApplied != nil
	l.announced = true
	l.mu.Unlock()
	if announce {
		l.onApplied()
	}
	return true
}

// handleBrokerError logs a failed broker call. A 404 means this session's
// token no longer names a live registration: the session re-registers (the
// heartbeat after an expired lease, or an access-host change) and publishes
// the new token through the session, so wait for that — at most one
// heartbeat — and start over with a full poll (the broker reset the applied
// revision when it minted the token). A token that was replaced while the
// call was in flight ends the wait at once.
func (l *leaseSync) handleBrokerError(ctx context.Context, what string, err error, changed <-chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	if relayruntime.IsRelayNotFound(err) {
		l.mu.Lock()
		l.full = true
		l.mu.Unlock()
		select {
		case <-changed:
			return false // a newer session exists; step retries at once
		default:
		}
		l.logf("lease %s: registration gone; waiting for the heartbeat to re-register", what)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-time.After(l.heartbeatInterval):
		}
		return false
	}
	l.logf("lease %s failed: %v", what, err)
	return false
}

// reconcile makes Xray accept exactly resp's leases. Expiry-only changes
// update the local expiry without an Xray call; it returns true only when
// every needed Xray call succeeded, so a partial result is never acked.
//
// The mutex is held per operation, never across the batch: each lease is
// re-evaluated against the installed set under the lock, acted on, and
// released before the next, so the expiry sweep interleaves with a slow
// batch instead of waiting behind it — and because every decision is made
// under the same lock as its Xray call, an expiry that removed a lease while
// the batch was in flight is seen by the batch and the lease re-installed.
func (l *leaseSync) reconcile(ctx context.Context, resp relay.LeaseSyncResponse, now time.Time) bool {
	ok := true
	desired := make(map[string]struct{}, len(resp.Leases))
	for _, entry := range resp.Leases {
		remaining := entry.ExpiresAt.Sub(resp.ServerTime)
		if remaining <= 0 {
			// Already expired by the broker's clock; nothing to install.
			continue
		}
		desired[entry.ID] = struct{}{}
		if !l.install(ctx, entry, now.Add(remaining)) {
			ok = false
		}
	}
	for _, id := range l.installedIDs() {
		if _, keep := desired[id]; keep {
			continue
		}
		if !l.remove(ctx, id, "removed", nil) {
			ok = false
		}
	}
	return ok
}

// install makes Xray accept entry, or just moves its local expiry when the
// same credential is already installed. One locked operation.
func (l *leaseSync) install(ctx context.Context, entry relay.LeaseEntry, localExpiry time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	email := relay.LeaseEmailPrefix + entry.ID
	if current, installed := l.installed[entry.ID]; installed && current.credential == entry.Credential {
		current.expiresAt = localExpiry
		l.installed[entry.ID] = current
		return true
	} else if installed {
		// A credential change (never expected: the broker keeps a lease's
		// UUID stable) is a remove-then-add under the same label.
		if err := l.users.RemoveUser(ctx, email); err != nil {
			l.logf("could not replace lease %s (will retry): %v", entry.ID, err)
			return false
		}
		delete(l.installed, entry.ID)
	}
	if err := l.users.AddUser(ctx, relayruntime.Credential{ID: entry.Credential, Email: email}); err != nil {
		l.logf("could not install lease %s (will retry): %v", entry.ID, err)
		return false
	}
	l.installed[entry.ID] = installedLease{credential: entry.Credential, expiresAt: localExpiry}
	l.logf("lease %s installed", entry.ID)
	return true
}

// remove drops lease id from Xray and the installed set in one locked
// operation. When unless is non-nil the removal is skipped if the installed
// entry no longer satisfies it (re-checked under the lock), so a sweep
// decision made from a snapshot cannot remove a lease that was extended in
// the meantime. A lease that is no longer installed is not an error.
func (l *leaseSync) remove(ctx context.Context, id, why string, unless func(installedLease) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lease, installed := l.installed[id]
	if !installed || (unless != nil && !unless(lease)) {
		return true
	}
	if err := l.users.RemoveUser(ctx, relay.LeaseEmailPrefix+id); err != nil {
		l.logf("could not remove lease %s (%s, will retry): %v", id, why, err)
		return false
	}
	delete(l.installed, id)
	l.logf("lease %s %s", id, why)
	return true
}

// installedIDs snapshots the installed lease IDs.
func (l *leaseSync) installedIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.installed))
	for id := range l.installed {
		ids = append(ids, id)
	}
	return ids
}

// expireLocal removes leases past their local expiry. Expiry is not a
// revision change on the broker, so this is the only thing that ends a lease
// the broker never revoked — including through a broker outage. It is one
// locked pass: the clock is read once the lock is held, so a wait behind a
// slow install cannot age the decision, and only leases that are expired at
// that moment cost an Xray call — unexpired entries are never waited on.
// A removal that fails is retried on the next tick.
func (l *leaseSync) expireLocal(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for id, lease := range l.installed {
		if lease.expiresAt.After(now) {
			continue
		}
		if err := l.users.RemoveUser(ctx, relay.LeaseEmailPrefix+id); err != nil {
			l.logf("could not expire lease %s (will retry): %v", id, err)
			continue
		}
		delete(l.installed, id)
		l.logf("lease %s expired", id)
	}
}

// removeAll drops every installed lease user, one locked operation each,
// and reports whether none is left. While draining the caller retries until
// it does; at session end it is one best-effort pass with a short deadline
// because the session context is already done.
func (l *leaseSync) removeAll(ctx context.Context) bool {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	drained := true
	for _, id := range l.installedIDs() {
		if !l.remove(ctx, id, "removed", nil) {
			drained = false
		}
	}
	return drained
}

// installedCount reports how many leases Xray currently accepts (status).
func (l *leaseSync) installedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.installed)
}
