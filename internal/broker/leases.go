// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"openrung/internal/relay"
)

// Access leases (ADR-006 "Client enrollment and relay authorization").
//
// A lease is one broker-issued, short-lived VLESS credential for one (user,
// relay) pair. Relays install leases through their Xray management API and
// learn the set they must accept through an outbound long poll; disabling a
// user revokes their leases, and the next poll removes the credentials.
//
// Synchronisation contract (shared by both stores; see the plan in the PR):
//   - every change to a relay's desired set — issue, renewal, revocation, cap
//     eviction — runs in one transaction that locks that relay's
//     relay_lease_state row, advances its revision by one and stamps the new
//     revision on every lease it touched;
//   - multi-relay writes lock the user first, compute the complete affected
//     relay set (requested relays and eviction victims' relays), then lock
//     those state rows in sorted ID order;
//   - the relay reads the set and the revision in one snapshot, so an ack of
//     revision N proves every lease with revision ≤ N was in a set the relay
//     applied;
//   - relay_lease_state outlives descriptors: Prune and the endpoint-move
//     delete in Register never touch it, and a registration that mints a new
//     relay lease token resets applied_revision to 0 so the relay's first poll
//     re-installs every live lease before any is relied upon.

const (
	defaultAccessLeaseTTL = time.Hour
	minAccessLeaseTTL     = time.Minute
	maxAccessLeaseTTL     = 24 * time.Hour

	defaultMaxLeasesPerUser  = 10
	defaultMaxLeasesPerRelay = 2000

	// leaseRetention keeps expired and revoked leases resolvable through the
	// operational API for as long as relay logs plausibly refer to them.
	leaseRetention = 7 * 24 * time.Hour

	leaseIDBytes = 16

	// leasePollHold is how long GET /api/v1/relays/{id}/leases?after=N waits
	// for a change before answering with the unchanged set. It sits well
	// under the broker's 2-minute server timeouts and CloudFront's 30 s origin
	// timeout (relays talk to the origin directly, but the margin is cheap).
	leasePollHold = 20 * time.Second
)

var (
	ErrUserDisabled  = errors.New("user is disabled")
	ErrLeaseNotFound = errors.New("lease not found")
	// ErrLeaseCapExceeded is returned when a request asks for more relays
	// than one user may hold live leases on, so no eviction could satisfy it.
	ErrLeaseCapExceeded = errors.New("request exceeds the per-user lease cap")
)

// Lease is one issued per-client credential.
type Lease struct {
	ID         string
	UserID     string
	RelayID    string
	Credential string
	CreatedAt  time.Time
	// RenewedAt is the last issue or extension; the per-user cap evicts the
	// least recently renewed leases first.
	RenewedAt time.Time
	ExpiresAt time.Time
	// RevokedAt is zero while the lease has not been revoked.
	RevokedAt time.Time
	// Revision is the relay's revision at this lease's last change.
	Revision int64
}

// Live reports whether the lease should currently be accepted.
func (l Lease) Live(now time.Time) bool {
	return l.RevokedAt.IsZero() && l.ExpiresAt.After(now)
}

// Email is the Xray user label relays install the lease under.
func (l Lease) Email() string {
	return relay.LeaseEmailPrefix + l.ID
}

// RelayLeaseSet is one consistent snapshot of a relay's desired set.
type RelayLeaseSet struct {
	Revision        int64
	AppliedRevision int64
	// Leases are the live leases, sorted by ID.
	Leases []Lease
}

// LeaseIssueRequest parameterises IssueLeases.
type LeaseIssueRequest struct {
	UserID string
	// RelayIDs are the relays the client wants a lease on; duplicates are
	// ignored.
	RelayIDs []string
	TTL      time.Duration
	// MaxPerUser caps a user's live leases across the fleet; MaxPerRelay caps
	// one relay's live leases. Zero applies the defaults.
	MaxPerUser  int
	MaxPerRelay int
}

func (r LeaseIssueRequest) normalized() LeaseIssueRequest {
	if r.TTL <= 0 {
		r.TTL = defaultAccessLeaseTTL
	}
	if r.MaxPerUser <= 0 {
		r.MaxPerUser = defaultMaxLeasesPerUser
	}
	if r.MaxPerRelay <= 0 {
		r.MaxPerRelay = defaultMaxLeasesPerRelay
	}
	r.RelayIDs = sortedUnique(r.RelayIDs)
	return r
}

// LeaseIssueResult is what IssueLeases hands back: the leases now live on the
// requested relays (a relay at its cap yields none) and every relay whose
// desired set changed, which the caller wakes.
type LeaseIssueResult struct {
	Leases []Lease
	// ChangedRelayIDs is sorted and includes eviction victims' relays.
	ChangedRelayIDs []string
}

// LeaseStore is the lease half of AccessStore, split out for documentation;
// AccessStore embeds it so one assertion covers both.
type LeaseStore interface {
	// IssueLeases gives the user a live lease on every requested relay in
	// one transaction: a live lease on a requested relay is extended to
	// now+TTL keeping its credential, otherwise — after evicting the least
	// recently renewed leases outside the request when the user is at
	// MaxPerUser — a new lease is inserted unless the relay already holds
	// MaxPerRelay live leases, in which case that relay is skipped.
	// ErrUserNotFound, ErrUserDisabled and ErrLeaseCapExceeded are the
	// refusals; nothing changes on any error.
	IssueLeases(ctx context.Context, req LeaseIssueRequest, now time.Time) (LeaseIssueResult, error)
	// RelayLeaseSet returns the relay's live leases and its revision in one
	// snapshot. The lease token must match the relay's current identity-
	// bearing registration; anything else (unknown relay, wrong token, legacy
	// identityless row) is ErrRelayNotFound.
	RelayLeaseSet(ctx context.Context, relayID, leaseToken string, now time.Time) (RelayLeaseSet, error)
	// AckLeaseRevision records that the relay applied the set at revision;
	// applied_revision only ever rises. Same guard as RelayLeaseSet.
	AckLeaseRevision(ctx context.Context, relayID, leaseToken string, revision int64, now time.Time) error
	// AppliedLeaseRevisions returns applied_revision per relay (0 for relays
	// without state).
	AppliedLeaseRevisions(ctx context.Context, relayIDs []string) (map[string]int64, error)
	// LeaseByID resolves a lease label to its record, live or not, for as
	// long as it is retained. ErrLeaseNotFound otherwise.
	LeaseByID(ctx context.Context, id string) (Lease, error)
}

// leaseStateRow is the in-memory relay_lease_state row.
type leaseStateRow struct {
	revision int64
	applied  int64
}

func newLeaseID() (string, error) {
	var b [leaseIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// wellFormedLeaseID reports whether id has the shape newLeaseID mints.
func wellFormedLeaseID(id string) bool {
	if len(id) != 2*leaseIDBytes {
		return false
	}
	for _, c := range []byte(id) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newLeaseCredential mints a random version-4 UUID, the VLESS client ID form
// xray accepts.
func newLeaseCredential() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func sortedUnique(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// leaseNotifier wakes long polls and ack waiters in process. It is a hint,
// not state: a woken waiter re-reads the store, which is the truth, and a
// missed wakeup costs at most one poll hold. Entries exist only while someone
// waits: a subscriber that leaves without a broadcast releases its
// subscription, so unauthenticated or abandoned requests retain nothing.
type leaseNotifier struct {
	mu      sync.Mutex
	changed map[string]*leaseWaiters
	applied map[string]*leaseWaiters
}

// leaseWaiters is one generation of subscribers to one key.
type leaseWaiters struct {
	ch   chan struct{}
	refs int
}

// leaseSubscription is one subscriber's handle: C closes on the next
// broadcast; release drops the subscription (idempotent, safe after a
// broadcast).
type leaseSubscription struct {
	C       <-chan struct{}
	release func()
}

func newLeaseNotifier() *leaseNotifier {
	return &leaseNotifier{changed: make(map[string]*leaseWaiters), applied: make(map[string]*leaseWaiters)}
}

// changedC subscribes to the next desired-set change of relayID. Subscribe
// before re-reading the set so a change between the read and the wait cannot
// be lost, and release the subscription when done waiting.
func (n *leaseNotifier) changedC(relayID string) leaseSubscription {
	return subscribe(&n.mu, n.changed, relayID)
}

// notifyChanged wakes every waiter on the relays' desired sets.
func (n *leaseNotifier) notifyChanged(relayIDs ...string) {
	broadcast(&n.mu, n.changed, relayIDs)
}

// appliedC subscribes to the next ack from relayID.
func (n *leaseNotifier) appliedC(relayID string) leaseSubscription {
	return subscribe(&n.mu, n.applied, relayID)
}

// notifyApplied wakes every waiter on the relay's applied revision.
func (n *leaseNotifier) notifyApplied(relayID string) {
	broadcast(&n.mu, n.applied, []string{relayID})
}

// waiting reports how many keys currently have subscribers (tests).
func (n *leaseNotifier) waiting() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.changed) + len(n.applied)
}

func subscribe(mu *sync.Mutex, waiters map[string]*leaseWaiters, key string) leaseSubscription {
	mu.Lock()
	defer mu.Unlock()
	entry, ok := waiters[key]
	if !ok {
		entry = &leaseWaiters{ch: make(chan struct{})}
		waiters[key] = entry
	}
	entry.refs++
	released := false
	return leaseSubscription{C: entry.ch, release: func() {
		mu.Lock()
		defer mu.Unlock()
		if released {
			return
		}
		released = true
		entry.refs--
		// Only the generation still registered is removable; a broadcast
		// already dropped and replaced closed ones.
		if entry.refs == 0 && waiters[key] == entry {
			delete(waiters, key)
		}
	}}
}

func broadcast(mu *sync.Mutex, waiters map[string]*leaseWaiters, keys []string) {
	mu.Lock()
	defer mu.Unlock()
	for _, key := range keys {
		if entry, ok := waiters[key]; ok {
			close(entry.ch)
			delete(waiters, key)
		}
	}
}
