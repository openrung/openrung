// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

// leaseTestStore is a store that can both register relays and hold leases;
// the in-memory Store and PostgresStore both qualify.
type leaseTestStore interface {
	RelayStore
	AccessStore
}

func TestLeaseStoreConformance(t *testing.T) {
	t.Run("memory", func(t *testing.T) { leaseStoreConformance(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { leaseStoreConformance(t, newTestPostgresStore(t)) })
}

func TestLeaseStoreSurvivesDescriptorDeletion(t *testing.T) {
	t.Run("memory", func(t *testing.T) { leaseStateSurvivesDescriptorDeletion(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { leaseStateSurvivesDescriptorDeletion(t, newTestPostgresStore(t)) })
}

func TestLeaseStoreConcurrency(t *testing.T) {
	t.Run("memory", func(t *testing.T) { leaseStoreConcurrency(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { leaseStoreConcurrency(t, newTestPostgresStore(t)) })
}

func TestLeaseStoreDeadlockFreedom(t *testing.T) {
	t.Run("memory", func(t *testing.T) { leaseStoreDeadlockFreedom(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { leaseStoreDeadlockFreedom(t, newTestPostgresStore(t)) })
}

// identitySeed returns a distinct, valid identity seed per letter.
func identitySeed(c byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{c}, 32))
}

// registerLeaseRelay registers an identity-bearing, lease-capable relay at
// its own endpoint and returns the descriptor (with its lease token).
func registerLeaseRelay(t *testing.T, store RelayStore, seed byte, host string, now time.Time) relay.Descriptor {
	t.Helper()
	req := signedIdentityRequest(t, identitySeed(seed), func(r *relay.RegisterRequest) {
		r.PublicHost = host
		r.AccessLeases = true
	}, now)
	desc, err := store.Register(req, now, 3*time.Minute)
	if err != nil {
		t.Fatalf("register relay %s: %v", host, err)
	}
	if desc.LeaseToken == "" || !desc.LeaseCapable {
		t.Fatalf("identity registration with access_leases: token %q, lease_capable %v", desc.LeaseToken, desc.LeaseCapable)
	}
	return desc
}

func enrollLeaseUser(t *testing.T, store AccessStore, now time.Time) User {
	t.Helper()
	canonical, err := brokerapi.CanonicalUserPublicKey(&newUserKey(t).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := store.AuthenticateUser(context.Background(), canonical, "lease-src", nil, now)
	if err != nil {
		t.Fatalf("enroll user: %v", err)
	}
	return user
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func leaseIDs(leases []Lease) []string {
	ids := make([]string, 0, len(leases))
	for _, lease := range leases {
		ids = append(ids, lease.ID)
	}
	sort.Strings(ids)
	return ids
}

func leaseStoreConformance(t *testing.T, store leaseTestStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	relayA := registerLeaseRelay(t, store, 'A', "a.example.com", now)
	relayB := registerLeaseRelay(t, store, 'B', "b.example.com", now)
	legacy, err := store.Register(validRegisterRequest(), now, 3*time.Minute)
	if err != nil {
		t.Fatalf("register legacy relay: %v", err)
	}
	if legacy.LeaseCapable {
		t.Fatal("a legacy identityless registration must never be lease-capable")
	}
	user := enrollLeaseUser(t, store, now)

	// A fresh relay has an empty set at revision 0; the guard refuses wrong
	// tokens and legacy rows alike with ErrRelayNotFound.
	set, err := store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, now)
	if err != nil || set.Revision != 0 || set.AppliedRevision != 0 || len(set.Leases) != 0 {
		t.Fatalf("fresh set = %+v, %v", set, err)
	}
	if _, err := store.RelayLeaseSet(ctx, relayA.ID, "lease_wrong", now); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := store.RelayLeaseSet(ctx, legacy.ID, "", now); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("legacy row: %v", err)
	}
	if _, err := store.RelayLeaseSet(ctx, "relay_"+strings.Repeat("0", 32), relayA.LeaseToken, now); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("unknown relay: %v", err)
	}

	// Issue on two relays: one lease each, both relays at revision 1.
	issue := func(userID string, relayIDs []string, at time.Time, maxPerUser, maxPerRelay int) (LeaseIssueResult, error) {
		return store.IssueLeases(ctx, LeaseIssueRequest{UserID: userID, RelayIDs: relayIDs, TTL: ttl, MaxPerUser: maxPerUser, MaxPerRelay: maxPerRelay}, at)
	}
	first, err := issue(user.ID, []string{relayB.ID, relayA.ID, relayA.ID}, now, 10, 2000)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(first.Leases) != 2 || !equalStrings(first.ChangedRelayIDs, sortedUnique([]string{relayA.ID, relayB.ID})) {
		t.Fatalf("first issue = %+v", first)
	}
	byRelay := map[string]Lease{}
	for _, lease := range first.Leases {
		byRelay[lease.RelayID] = lease
		if !wellFormedLeaseID(lease.ID) || !uuidV4.MatchString(lease.Credential) || lease.UserID != user.ID {
			t.Fatalf("lease shape = %+v", lease)
		}
		if lease.Revision != 1 || !lease.CreatedAt.Equal(now) || !lease.RenewedAt.Equal(now) || !lease.ExpiresAt.Equal(now.Add(ttl)) || !lease.RevokedAt.IsZero() {
			t.Fatalf("lease stamps = %+v", lease)
		}
	}
	set, err = store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, now)
	if err != nil || set.Revision != 1 || len(set.Leases) != 1 || set.Leases[0].ID != byRelay[relayA.ID].ID {
		t.Fatalf("relay A set = %+v, %v", set, err)
	}

	// Renewal keeps the credential and the ID, extends the expiry, advances
	// only the renewed relay.
	later := now.Add(10 * time.Minute)
	renewed, err := issue(user.ID, []string{relayA.ID}, later, 10, 2000)
	if err != nil || len(renewed.Leases) != 1 || !equalStrings(renewed.ChangedRelayIDs, []string{relayA.ID}) {
		t.Fatalf("renew = %+v, %v", renewed, err)
	}
	if got, want := renewed.Leases[0], byRelay[relayA.ID]; got.ID != want.ID || got.Credential != want.Credential || !got.ExpiresAt.Equal(later.Add(ttl)) || !got.RenewedAt.Equal(later) || !got.CreatedAt.Equal(now) || got.Revision != 2 {
		t.Fatalf("renewed lease = %+v, original %+v", got, want)
	}
	set, _ = store.RelayLeaseSet(ctx, relayB.ID, relayB.LeaseToken, later)
	if set.Revision != 1 {
		t.Fatalf("relay B revision after renewing A = %d, want 1", set.Revision)
	}

	// Per-user cap: with 2 live leases (A renewed later, B older) and cap 3,
	// asking for two new relays evicts exactly one — the least recently
	// renewed lease outside the request, B — and bumps B's revision.
	relayC, relayD := "relay_"+strings.Repeat("c", 32), "relay_"+strings.Repeat("d", 32)
	capped, err := issue(user.ID, []string{relayC, relayD}, later.Add(time.Minute), 3, 2000)
	if err != nil || len(capped.Leases) != 2 {
		t.Fatalf("capped issue = %+v, %v", capped, err)
	}
	if !equalStrings(capped.ChangedRelayIDs, sortedUnique([]string{relayB.ID, relayC, relayD})) {
		t.Fatalf("changed relays = %v, want B (victim), C, D", capped.ChangedRelayIDs)
	}
	evicted, err := store.LeaseByID(ctx, byRelay[relayB.ID].ID)
	if err != nil || !evicted.RevokedAt.Equal(later.Add(time.Minute)) || evicted.Revision != 2 {
		t.Fatalf("evicted lease = %+v, %v", evicted, err)
	}
	set, _ = store.RelayLeaseSet(ctx, relayB.ID, relayB.LeaseToken, later.Add(time.Minute))
	if set.Revision != 2 || len(set.Leases) != 0 {
		t.Fatalf("relay B after eviction = %+v", set)
	}
	if stillA, _ := store.LeaseByID(ctx, byRelay[relayA.ID].ID); !stillA.RevokedAt.IsZero() {
		t.Fatal("the most recently renewed lease was evicted")
	}
	// A request wider than the cap cannot be satisfied by any eviction.
	if _, err := issue(user.ID, []string{relayA.ID, relayB.ID, relayC, relayD}, later, 3, 2000); !errors.Is(err, ErrLeaseCapExceeded) {
		t.Fatalf("over-wide request: %v", err)
	}

	// Per-relay cap: a relay at its cap is skipped, with no revision change.
	other := enrollLeaseUser(t, store, now)
	before, _ := store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, later)
	full, err := issue(other.ID, []string{relayA.ID}, later, 10, 1)
	if err != nil || len(full.Leases) != 0 || len(full.ChangedRelayIDs) != 0 {
		t.Fatalf("issue on a full relay = %+v, %v", full, err)
	}
	after, _ := store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, later)
	if after.Revision != before.Revision {
		t.Fatalf("a skipped relay advanced from %d to %d", before.Revision, after.Revision)
	}
	// A skipped relay costs the user nothing: at cap 1 with a live lease on
	// relay E, asking for full relay A evicts nothing and issues nothing.
	relayE := "relay_" + strings.Repeat("e", 32)
	held, err := issue(other.ID, []string{relayE}, later, 1, 2000)
	if err != nil || len(held.Leases) != 1 {
		t.Fatalf("seed lease on E = %+v, %v", held, err)
	}
	none, err := issue(other.ID, []string{relayA.ID}, later.Add(time.Second), 1, 1)
	if err != nil || len(none.Leases) != 0 || len(none.ChangedRelayIDs) != 0 {
		t.Fatalf("issue on a full relay at the user cap = %+v, %v", none, err)
	}
	if kept, _ := store.LeaseByID(ctx, held.Leases[0].ID); !kept.RevokedAt.IsZero() {
		t.Fatal("a lease was evicted to make room for a relay that could not take one")
	}
	// Mixed request at cap 2 with live leases on E (older) and G: one full
	// relay, one with room — exactly one eviction, of the oldest lease.
	relayF, relayG := "relay_"+strings.Repeat("f", 32), "relay_"+strings.Repeat("1", 32)
	heldG, err := issue(other.ID, []string{relayG}, later.Add(2*time.Second), 2, 2000)
	if err != nil || len(heldG.Leases) != 1 {
		t.Fatalf("seed lease on G = %+v, %v", heldG, err)
	}
	mixed, err := issue(other.ID, []string{relayA.ID, relayF}, later.Add(3*time.Second), 2, 1)
	if err != nil || len(mixed.Leases) != 1 || mixed.Leases[0].RelayID != relayF {
		t.Fatalf("mixed issue = %+v, %v", mixed, err)
	}
	if evictedE, _ := store.LeaseByID(ctx, held.Leases[0].ID); evictedE.RevokedAt.IsZero() {
		t.Fatal("the issuable create did not evict the oldest lease")
	}
	if keptG, _ := store.LeaseByID(ctx, heldG.Leases[0].ID); !keptG.RevokedAt.IsZero() {
		t.Fatal("the full relay's skipped create evicted a second lease")
	}
	if !equalStrings(mixed.ChangedRelayIDs, sortedUnique([]string{relayE, relayF})) {
		t.Fatalf("mixed changed relays = %v", mixed.ChangedRelayIDs)
	}

	// Acks only rise and share the guard.
	if err := store.AckLeaseRevision(ctx, relayA.ID, relayA.LeaseToken, 2, later); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := store.AckLeaseRevision(ctx, relayA.ID, relayA.LeaseToken, 1, later); err != nil {
		t.Fatalf("lower ack: %v", err)
	}
	if err := store.AckLeaseRevision(ctx, relayA.ID, "lease_wrong", 3, later); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("ack with wrong token: %v", err)
	}
	if err := store.AckLeaseRevision(ctx, legacy.ID, "", 1, later); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("ack on legacy row: %v", err)
	}
	applied, err := store.AppliedLeaseRevisions(ctx, []string{relayA.ID, relayB.ID, "relay_" + strings.Repeat("9", 32)})
	if err != nil || applied[relayA.ID] != 2 || applied[relayB.ID] != 0 || applied["relay_"+strings.Repeat("9", 32)] != 0 || len(applied) != 3 {
		t.Fatalf("applied = %v, %v", applied, err)
	}
	set, _ = store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, later)
	if set.AppliedRevision != 2 {
		t.Fatalf("set applied = %d, want 2", set.AppliedRevision)
	}
	if _, err := store.LeaseByID(ctx, strings.Repeat("0", 32)); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("unknown lease: %v", err)
	}

	// Disabling revokes every live lease (A, C, D; B is already gone) one
	// revision step per relay, in sorted order, and refuses later issuance.
	disabledAt := later.Add(2 * time.Minute)
	_, changed, err := store.SetUserStatus(ctx, user.ID, brokerapi.UserStatusDisabled, "test", disabledAt)
	if err != nil || !equalStrings(changed, sortedUnique([]string{relayA.ID, relayC, relayD})) {
		t.Fatalf("disable changed = %v, %v", changed, err)
	}
	set, _ = store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, disabledAt)
	if set.Revision != 3 || len(set.Leases) != 0 {
		t.Fatalf("relay A after disable = %+v", set)
	}
	revoked, _ := store.LeaseByID(ctx, byRelay[relayA.ID].ID)
	if !revoked.RevokedAt.Equal(disabledAt) || revoked.Revision != 3 {
		t.Fatalf("revoked lease = %+v", revoked)
	}
	if _, err := issue(user.ID, []string{relayA.ID}, disabledAt, 10, 2000); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("issue while disabled: %v", err)
	}
	if _, err := issue("user_"+strings.Repeat("0", 32), []string{relayA.ID}, disabledAt, 10, 2000); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("issue for unknown user: %v", err)
	}
	// Re-enabling revokes nothing more and changes no relay.
	if _, changed, err := store.SetUserStatus(ctx, user.ID, brokerapi.UserStatusEnabled, "", disabledAt.Add(time.Minute)); err != nil || len(changed) != 0 {
		t.Fatalf("enable changed = %v, %v", changed, err)
	}

	// Expiry is local to the relay (no revision), and retention keeps the
	// record resolvable for a week before Prune drops it.
	expired, err := issue(user.ID, []string{relayA.ID}, disabledAt.Add(2*time.Minute), 10, 2000)
	if err != nil || len(expired.Leases) != 1 {
		t.Fatalf("issue after enable = %+v, %v", expired, err)
	}
	pastExpiry := disabledAt.Add(2*time.Minute + ttl + time.Second)
	set, _ = store.RelayLeaseSet(ctx, relayA.ID, relayA.LeaseToken, pastExpiry)
	if set.Revision != 4 || len(set.Leases) != 0 {
		t.Fatalf("relay A after expiry = %+v", set)
	}
	if _, err := store.Prune(pastExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LeaseByID(ctx, expired.Leases[0].ID); err != nil {
		t.Fatalf("expired lease pruned inside its retention: %v", err)
	}
	if _, err := store.Prune(pastExpiry.Add(leaseRetention)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LeaseByID(ctx, expired.Leases[0].ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("lease past retention: %v", err)
	}
	// The relay re-registers after that prune and finds its revision intact.
	again := registerLeaseRelay(t, store, 'A', "a.example.com", pastExpiry.Add(leaseRetention))
	set, err = store.RelayLeaseSet(ctx, relayA.ID, again.LeaseToken, pastExpiry.Add(leaseRetention))
	if err != nil || set.Revision != 4 || set.AppliedRevision != 0 {
		t.Fatalf("state after prune and re-registration = %+v, %v", set, err)
	}
}

// Every path that deletes or replaces a descriptor — a fresh registration
// of the same identity, an endpoint move, and Prune of an expired descriptor
// — leaves relay_lease_state and the live leases intact and resets
// applied_revision to 0, so the relay's first poll after it re-installs the
// surviving leases and nothing is released against the stale ack.
func leaseStateSurvivesDescriptorDeletion(t *testing.T, store leaseTestStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	user := enrollLeaseUser(t, store, now)

	paths := []struct {
		name string
		seed byte
		host string
		// disrupt deletes or replaces the descriptor and returns the time of
		// the re-registration and the re-registered descriptor.
		disrupt func(t *testing.T, desc relay.Descriptor) (relay.Descriptor, time.Time)
	}{
		{"fresh registration", 'E', "e.example.com", func(t *testing.T, desc relay.Descriptor) (relay.Descriptor, time.Time) {
			at := now.Add(time.Minute)
			return registerLeaseRelay(t, store, 'E', "e.example.com", at), at
		}},
		{"endpoint move", 'F', "f.example.com", func(t *testing.T, desc relay.Descriptor) (relay.Descriptor, time.Time) {
			at := now.Add(time.Minute)
			moved := registerLeaseRelay(t, store, 'F', "f2.example.com", at)
			if moved.ID != desc.ID {
				t.Fatalf("moved relay changed ID: %s → %s", desc.ID, moved.ID)
			}
			if _, err := store.RelayLeaseSet(ctx, desc.ID, desc.LeaseToken, at); !errors.Is(err, ErrRelayNotFound) {
				t.Fatalf("old token after the move: %v", err)
			}
			return moved, at
		}},
		{"prune", 'G', "g.example.com", func(t *testing.T, desc relay.Descriptor) (relay.Descriptor, time.Time) {
			at := now.Add(5 * time.Minute)
			pruned, err := store.Prune(at)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range pruned {
				found = found || p.ID == desc.ID
			}
			if !found {
				t.Fatalf("Prune did not drop the expired descriptor %s", desc.ID)
			}
			if _, err := store.RelayLeaseSet(ctx, desc.ID, desc.LeaseToken, at); !errors.Is(err, ErrRelayNotFound) {
				t.Fatalf("pruned relay still readable: %v", err)
			}
			return registerLeaseRelay(t, store, 'G', "g.example.com", at), at
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			desc := registerLeaseRelay(t, store, path.seed, path.host, now)
			issued, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: user.ID, RelayIDs: []string{desc.ID}, TTL: time.Hour}, now)
			if err != nil || len(issued.Leases) != 1 {
				t.Fatalf("issue: %+v, %v", issued, err)
			}
			if err := store.AckLeaseRevision(ctx, desc.ID, desc.LeaseToken, 1, now); err != nil {
				t.Fatal(err)
			}
			set, _ := store.RelayLeaseSet(ctx, desc.ID, desc.LeaseToken, now)
			if set.Revision != 1 || set.AppliedRevision != 1 {
				t.Fatalf("before disruption = %+v", set)
			}

			again, at := path.disrupt(t, desc)
			if again.LeaseToken == desc.LeaseToken {
				t.Fatal("re-registration kept the lease token")
			}
			set, err = store.RelayLeaseSet(ctx, desc.ID, again.LeaseToken, at)
			if err != nil {
				t.Fatalf("set after %s: %v", path.name, err)
			}
			if set.Revision != 1 || set.AppliedRevision != 0 {
				t.Fatalf("after %s: revision %d applied %d, want revision kept at 1 and applied reset to 0", path.name, set.Revision, set.AppliedRevision)
			}
			if !equalStrings(leaseIDs(set.Leases), leaseIDs(issued.Leases)) {
				t.Fatalf("after %s: leases %v, want the surviving %v", path.name, leaseIDs(set.Leases), leaseIDs(issued.Leases))
			}
			applied, _ := store.AppliedLeaseRevisions(ctx, []string{desc.ID})
			if applied[desc.ID] != 0 {
				t.Fatalf("applied after %s = %d", path.name, applied[desc.ID])
			}
			// The stale session's ack no longer counts.
			if err := store.AckLeaseRevision(ctx, desc.ID, desc.LeaseToken, 1, at); !errors.Is(err, ErrRelayNotFound) {
				t.Fatalf("stale ack after %s: %v", path.name, err)
			}
		})
	}
}

// Parallel issuance for one (user, relay) yields one lease; parallel
// issuance and revocation on one relay yield strictly increasing revisions,
// and every snapshot a relay reads is consistent with the final state of
// every lease stamped at or below the snapshot's revision — the property an
// ack of N relies on.
func leaseStoreConcurrency(t *testing.T, store leaseTestStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	target := registerLeaseRelay(t, store, 'H', "h.example.com", now)

	user := enrollLeaseUser(t, store, now)
	var wg sync.WaitGroup
	results := make([]LeaseIssueResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.IssueLeases(ctx, LeaseIssueRequest{UserID: user.ID, RelayIDs: []string{target.ID}, TTL: time.Hour}, now.Add(time.Duration(i)*time.Second))
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || len(results[i].Leases) != 1 || results[i].Leases[0].ID != results[0].Leases[0].ID {
			t.Fatalf("parallel issue %d = %+v, %v", i, results[i], errs[i])
		}
	}
	set, _ := store.RelayLeaseSet(ctx, target.ID, target.LeaseToken, now)
	if len(set.Leases) != 1 || set.Revision != 8 {
		t.Fatalf("after parallel issue for one user: %d leases at revision %d", len(set.Leases), set.Revision)
	}

	// Writers: 12 users each issue then get disabled; a reader polls the set
	// and acks like a relay would. Every change is one revision step.
	const writers = 12
	users := make([]User, writers)
	for i := range users {
		users[i] = enrollLeaseUser(t, store, now)
	}
	type snapshot struct {
		revision int64
		ids      map[string]struct{}
	}
	var snapshots []snapshot
	stopReader := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		var lastAcked int64
		for {
			select {
			case <-stopReader:
				readerDone <- nil
				return
			default:
			}
			set, err := store.RelayLeaseSet(ctx, target.ID, target.LeaseToken, now)
			if err != nil {
				readerDone <- err
				return
			}
			ids := make(map[string]struct{}, len(set.Leases))
			for _, lease := range set.Leases {
				ids[lease.ID] = struct{}{}
			}
			snapshots = append(snapshots, snapshot{revision: set.Revision, ids: ids})
			if set.Revision > lastAcked {
				if err := store.AckLeaseRevision(ctx, target.ID, target.LeaseToken, set.Revision, now); err != nil {
					readerDone <- err
					return
				}
				lastAcked = set.Revision
			}
		}
	}()

	var mu sync.Mutex
	var revisions []int64
	var issuedIDs []string
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: users[i].ID, RelayIDs: []string{target.ID}, TTL: time.Hour}, now)
			if err != nil {
				t.Errorf("writer %d issue: %v", i, err)
				return
			}
			mu.Lock()
			revisions = append(revisions, result.Leases[0].Revision)
			issuedIDs = append(issuedIDs, result.Leases[0].ID)
			mu.Unlock()
			if i%2 == 0 {
				_, _, err := store.SetUserStatus(ctx, users[i].ID, brokerapi.UserStatusDisabled, "race", now)
				if err != nil {
					t.Errorf("writer %d disable: %v", i, err)
					return
				}
				revoked, err := store.LeaseByID(ctx, result.Leases[0].ID)
				if err != nil {
					t.Errorf("writer %d lease: %v", i, err)
					return
				}
				mu.Lock()
				revisions = append(revisions, revoked.Revision)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	close(stopReader)
	if err := <-readerDone; err != nil {
		t.Fatalf("reader: %v", err)
	}

	// Strictly increasing: every change got its own revision and together
	// they are exactly the steps after the first phase's 8.
	sort.Slice(revisions, func(i, j int) bool { return revisions[i] < revisions[j] })
	for i, revision := range revisions {
		if revision != 8+int64(i)+1 {
			t.Fatalf("revisions %v are not the consecutive steps after 8", revisions)
		}
	}
	final, _ := store.RelayLeaseSet(ctx, target.ID, target.LeaseToken, now)
	if final.Revision != 8+int64(len(revisions)) || final.AppliedRevision > final.Revision {
		t.Fatalf("final state = revision %d applied %d, want %d", final.Revision, final.AppliedRevision, 8+len(revisions))
	}
	// Consistency: a lease whose last change is ≤ a snapshot's revision must
	// appear in that snapshot exactly when its final state is live.
	var last int64 = -1
	for _, snap := range snapshots {
		if snap.revision < last {
			t.Fatalf("snapshot revisions went backwards: %d after %d", snap.revision, last)
		}
		last = snap.revision
		for _, id := range issuedIDs {
			lease, err := store.LeaseByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if lease.Revision > snap.revision {
				continue
			}
			_, present := snap.ids[id]
			if live := lease.RevokedAt.IsZero(); present != live {
				t.Fatalf("snapshot at revision %d has lease %s present=%v, but its final state (revision %d) is live=%v", snap.revision, id, present, lease.Revision, live)
			}
		}
	}
}

// Two users whose requests and eviction victims overlap on the same relays,
// in opposite orders, must never deadlock: both stores take the state locks
// in sorted ID order after the user lock.
func leaseStoreDeadlockFreedom(t *testing.T, store leaseTestStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	relays := make([]string, 6)
	for i := range relays {
		relays[i] = "relay_" + strings.Repeat(fmt.Sprintf("%d", i), 32)
	}
	x, y := enrollLeaseUser(t, store, now), enrollLeaseUser(t, store, now)
	// X holds 0,1,2 and Y holds 2,3,4 at cap 3; X then asks for 4,5 (evicting
	// among 0,1,2) while Y asks for 0,5 (evicting among 2,3,4): the affected
	// sets overlap on 0, 2, 4 and 5 from both sides.
	seed := func(user User, ids []string) {
		for i, id := range ids {
			if _, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: user.ID, RelayIDs: []string{id}, TTL: time.Hour, MaxPerUser: 3}, now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for round := 0; round < 20; round++ {
			at := now.Add(time.Duration(round) * time.Minute)
			seed(x, relays[0:3])
			seed(y, relays[2:5])
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: x.ID, RelayIDs: []string{relays[4], relays[5]}, TTL: time.Hour, MaxPerUser: 3}, at.Add(10*time.Second)); err != nil {
					t.Errorf("round %d X: %v", round, err)
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: y.ID, RelayIDs: []string{relays[0], relays[5]}, TTL: time.Hour, MaxPerUser: 3}, at.Add(10*time.Second)); err != nil {
					t.Errorf("round %d Y: %v", round, err)
				}
			}()
			wg.Wait()
			for _, user := range []User{x, y} {
				if _, _, err := store.SetUserStatus(ctx, user.ID, brokerapi.UserStatusDisabled, "reset", at.Add(20*time.Second)); err != nil {
					t.Errorf("round %d reset: %v", round, err)
				}
				if _, _, err := store.SetUserStatus(ctx, user.ID, brokerapi.UserStatusEnabled, "", at.Add(21*time.Second)); err != nil {
					t.Errorf("round %d reset: %v", round, err)
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("overlapping issuance deadlocked")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLeaseNotifierBroadcastsPerKeyAndReleases(t *testing.T) {
	n := newLeaseNotifier()
	a1, a2, b := n.changedC("a"), n.changedC("a"), n.changedC("b")
	if a1.C != a2.C {
		t.Fatal("two subscribers before a broadcast must share one generation")
	}
	n.notifyChanged("a")
	select {
	case <-a1.C:
	default:
		t.Fatal("a subscriber was not woken")
	}
	select {
	case <-b.C:
		t.Fatal("an unrelated key was woken")
	default:
	}
	a3 := n.changedC("a")
	if a3.C == a1.C {
		t.Fatal("a woken generation was reused")
	}
	// Releasing after a broadcast is harmless and must not disturb the new
	// generation; releasing the last live subscriber drops the entry.
	a1.release()
	a2.release()
	a1.release()
	if n.waiting() != 2 {
		t.Fatalf("waiting = %d after releasing a closed generation, want a3 and b", n.waiting())
	}
	a3.release()
	b.release()
	if n.waiting() != 0 {
		t.Fatalf("waiting = %d after releasing everything", n.waiting())
	}
	n.notifyChanged("unknown") // no waiter: nothing to do, must not panic
	applied := n.appliedC("a")
	n.notifyApplied("a")
	<-applied.C
	applied.release()
	if n.waiting() != 0 {
		t.Fatalf("waiting = %d", n.waiting())
	}
}

func TestPlanLeaseIssueEvictsLeastRecentlyRenewedOutsideRequest(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	live := []Lease{
		{ID: "1", RelayID: "r1", RenewedAt: now.Add(3 * time.Minute)},
		{ID: "2", RelayID: "r2", RenewedAt: now.Add(time.Minute)},
		{ID: "3", RelayID: "r3", RenewedAt: now.Add(2 * time.Minute)},
		{ID: "4", RelayID: "r4", RenewedAt: now.Add(time.Minute)},
	}
	plan, err := planLeaseIssue(LeaseIssueRequest{RelayIDs: []string{"r1", "r5", "r6"}, MaxPerUser: 4}.normalized(), live)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.extend["r1"]; !ok || !equalStrings(plan.create, []string{"r5", "r6"}) {
		t.Fatalf("extend/create = %v / %v", plan.extend, plan.create)
	}
	// Every relay an eviction could touch is in the lock set.
	if !equalStrings(plan.affected, []string{"r1", "r2", "r3", "r4", "r5", "r6"}) {
		t.Fatalf("affected = %v", plan.affected)
	}
	// 4 live + 2 issuable − cap 4 = 2 victims: the two renewed earliest
	// outside the request, ties broken by ID (2 then 4, never the requested r1).
	victims := plan.victims(map[string]bool{"r5": true, "r6": true})
	if len(victims["r2"]) != 1 || len(victims["r4"]) != 1 || len(victims) != 2 {
		t.Fatalf("victims = %v", victims)
	}
	// A relay without room is not issuable and costs no eviction.
	if victims := plan.victims(map[string]bool{"r5": true}); len(victims) != 1 || len(victims["r2"]) != 1 {
		t.Fatalf("victims with one full relay = %v", victims)
	}
	if victims := plan.victims(map[string]bool{}); len(victims) != 0 {
		t.Fatalf("victims with every relay full = %v", victims)
	}
	if _, err := planLeaseIssue(LeaseIssueRequest{RelayIDs: []string{"a", "b", "c"}, MaxPerUser: 2}.normalized(), nil); !errors.Is(err, ErrLeaseCapExceeded) {
		t.Fatalf("over-wide = %v", err)
	}
}
