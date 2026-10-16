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
// generation counter is how it notices the change.
type leaseSession struct {
	mu         sync.Mutex
	id         string
	token      string
	generation uint64
}

func (s *leaseSession) set(id, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.id && token == s.token {
		return
	}
	s.id, s.token = id, token
	s.generation++
}

func (s *leaseSession) get() (id, token string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id, s.token, s.generation
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
	}
}

// run polls, reconciles and acks until ctx ends, then removes every lease
// user it installed so a session that ends leaves no stray credential. The
// expiry sweep runs on its own ticker so an unreachable broker (a poll
// blocked in backoff) never delays local expiry.
func (l *leaseSync) run(ctx context.Context) {
	expiry := time.NewTicker(leaseExpiryInterval)
	defer expiry.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-expiry.C:
				l.expireLocal(ctx, l.now())
			}
		}
	}()

	backoff := leaseSyncMinBackoff
	for {
		if ctx.Err() != nil {
			l.removeAll()
			return
		}
		ok := l.step(ctx)
		if ok {
			backoff = leaseSyncMinBackoff
			continue
		}
		if ctx.Err() != nil {
			l.removeAll()
			return
		}
		select {
		case <-ctx.Done():
			l.removeAll()
			return
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
	id, token, generation := l.session.get()
	if id == "" || token == "" {
		return false
	}
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
		if !l.ack(ctx, id, token, applied) {
			return false
		}
	}

	after := int64(-1)
	if !full {
		after = applied
	}
	resp, err := l.broker.PollLeases(ctx, id, token, after)
	if err != nil {
		return l.handleBrokerError(ctx, "poll", err)
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
	changed := resp.Revision != l.applied
	l.applied = resp.Revision
	if full || changed || l.pendingAck {
		l.pendingAck = true
	}
	l.full = false
	pending = l.pendingAck
	l.mu.Unlock()
	if pending {
		return l.ack(ctx, id, token, resp.Revision)
	}
	return true
}

// ack confirms revision to the broker and, on success, clears the pending
// ack and fires onApplied once per syncer. Every ack — first attempt or
// retry — goes through here so the online transition cannot be skipped.
func (l *leaseSync) ack(ctx context.Context, id, token string, revision int64) bool {
	if err := l.broker.AckLeases(ctx, id, token, revision); err != nil {
		return l.handleBrokerError(ctx, "ack", err)
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
// token no longer names a live registration: the heartbeat loop will
// re-register and publish the new token through the session, so wait one
// heartbeat and start over with a full poll (the broker reset the applied
// revision when it minted the token).
func (l *leaseSync) handleBrokerError(ctx context.Context, what string, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if relayruntime.IsRelayNotFound(err) {
		l.logf("lease %s: registration gone; waiting for the heartbeat to re-register", what)
		l.mu.Lock()
		l.full = true
		l.mu.Unlock()
		select {
		case <-ctx.Done():
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
func (l *leaseSync) reconcile(ctx context.Context, resp relay.LeaseSyncResponse, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok := true
	desired := make(map[string]struct{}, len(resp.Leases))
	for _, entry := range resp.Leases {
		desired[entry.ID] = struct{}{}
		remaining := entry.ExpiresAt.Sub(resp.ServerTime)
		if remaining <= 0 {
			// Already expired by the broker's clock; nothing to install.
			delete(desired, entry.ID)
			continue
		}
		localExpiry := now.Add(remaining)
		email := relay.LeaseEmailPrefix + entry.ID
		if current, installed := l.installed[entry.ID]; installed && current.credential == entry.Credential {
			current.expiresAt = localExpiry
			l.installed[entry.ID] = current
			continue
		} else if installed {
			// A credential change (never expected: the broker keeps a lease's
			// UUID stable) is a remove-then-add under the same label.
			if err := l.users.RemoveUser(ctx, email); err != nil {
				l.logf("could not replace lease %s (will retry): %v", entry.ID, err)
				ok = false
				continue
			}
			delete(l.installed, entry.ID)
		}
		if err := l.users.AddUser(ctx, relayruntime.Credential{ID: entry.Credential, Email: email}); err != nil {
			l.logf("could not install lease %s (will retry): %v", entry.ID, err)
			ok = false
			continue
		}
		l.installed[entry.ID] = installedLease{credential: entry.Credential, expiresAt: localExpiry}
		l.logf("lease %s installed", entry.ID)
	}
	for id := range l.installed {
		if _, keep := desired[id]; keep {
			continue
		}
		if err := l.users.RemoveUser(ctx, relay.LeaseEmailPrefix+id); err != nil {
			l.logf("could not remove lease %s (will retry): %v", id, err)
			ok = false
			continue
		}
		delete(l.installed, id)
		l.logf("lease %s removed", id)
	}
	return ok
}

// expireLocal removes leases past their local expiry. Expiry is not a
// revision change on the broker, so this is the only thing that ends a lease
// the broker never revoked — including through a broker outage.
func (l *leaseSync) expireLocal(ctx context.Context, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

// removeAll drops every installed lease user when the session ends. It uses
// a short background context because the session context is already done.
func (l *leaseSync) removeAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.installed {
		if err := l.users.RemoveUser(ctx, relay.LeaseEmailPrefix+id); err != nil {
			l.logf("could not remove lease %s at session end: %v", id, err)
			continue
		}
		delete(l.installed, id)
	}
}

// installedCount reports how many leases Xray currently accepts (status).
func (l *leaseSync) installedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.installed)
}
