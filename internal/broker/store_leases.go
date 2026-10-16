// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"crypto/subtle"
	"sort"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// In-memory LeaseStore. Every method runs under the store's single mutex, so
// each write is one atomic step exactly like the Postgres transaction it
// mirrors; the per-relay revision bookkeeping below is what both stores share.

// leaseStateLocked returns the relay's state row, creating it at revision 0.
func (s *Store) leaseStateLocked(relayID string) *leaseStateRow {
	state, ok := s.leaseStates[relayID]
	if !ok {
		state = &leaseStateRow{}
		s.leaseStates[relayID] = state
	}
	return state
}

// liveLeasesForUserLocked returns the user's live leases sorted by ID.
func (s *Store) liveLeasesForUserLocked(userID string, now time.Time) []Lease {
	var live []Lease
	for _, lease := range s.leases {
		if lease.UserID == userID && lease.Live(now) {
			live = append(live, lease)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live
}

func (s *Store) liveLeaseCountForRelayLocked(relayID string, now time.Time) int {
	count := 0
	for _, lease := range s.leases {
		if lease.RelayID == relayID && lease.Live(now) {
			count++
		}
	}
	return count
}

func (s *Store) IssueLeases(_ context.Context, req LeaseIssueRequest, now time.Time) (LeaseIssueResult, error) {
	req = req.normalized()
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[req.UserID]
	if !ok {
		return LeaseIssueResult{}, ErrUserNotFound
	}
	if user.Status != brokerapi.UserStatusEnabled {
		return LeaseIssueResult{}, ErrUserDisabled
	}

	plan, err := planLeaseIssue(req, s.liveLeasesForUserLocked(req.UserID, now))
	if err != nil {
		return LeaseIssueResult{}, err
	}

	// Mint everything that can fail before touching state, so an error leaves
	// the store exactly as it was (the Postgres transaction rolls back).
	minted := make(map[string]Lease, len(plan.create))
	for _, relayID := range plan.create {
		id, err := newLeaseID()
		if err != nil {
			return LeaseIssueResult{}, err
		}
		credential, err := newLeaseCredential()
		if err != nil {
			return LeaseIssueResult{}, err
		}
		minted[relayID] = Lease{ID: id, UserID: req.UserID, RelayID: relayID, Credential: credential, CreatedAt: now, RenewedAt: now, ExpiresAt: now.Add(req.TTL)}
	}

	var result LeaseIssueResult
	for _, relayID := range plan.affected {
		state := s.leaseStateLocked(relayID)
		revision := state.revision + 1
		changed := false
		for _, victim := range plan.victims[relayID] {
			lease := s.leases[victim.ID]
			lease.RevokedAt = now
			lease.Revision = revision
			s.leases[lease.ID] = lease
			changed = true
		}
		if extend, ok := plan.extend[relayID]; ok {
			lease := s.leases[extend.ID]
			lease.RenewedAt = now
			lease.ExpiresAt = now.Add(req.TTL)
			lease.Revision = revision
			s.leases[lease.ID] = lease
			result.Leases = append(result.Leases, lease)
			changed = true
		}
		if lease, ok := minted[relayID]; ok {
			// Victims on this relay were just revoked, so they no longer count.
			if s.liveLeaseCountForRelayLocked(relayID, now) < req.MaxPerRelay {
				lease.Revision = revision
				s.leases[lease.ID] = lease
				result.Leases = append(result.Leases, lease)
				changed = true
			}
		}
		if changed {
			state.revision = revision
			result.ChangedRelayIDs = append(result.ChangedRelayIDs, relayID)
		}
	}
	return result, nil
}

// leaseIssuePlan is the per-relay work IssueLeases derived from the user's
// live leases: which to extend, where to create, and which to evict.
type leaseIssuePlan struct {
	extend  map[string]Lease   // requested relay → the live lease to extend
	create  []string           // requested relays without a live lease
	victims map[string][]Lease // relay → leases evicted for the user cap
	// affected is requested ∪ victims' relays, sorted: the lock order.
	affected []string
}

// planLeaseIssue is the one decision procedure both stores run after locking
// the user and reading their live leases.
func planLeaseIssue(req LeaseIssueRequest, live []Lease) (leaseIssuePlan, error) {
	plan := leaseIssuePlan{extend: make(map[string]Lease), victims: make(map[string][]Lease)}
	requested := make(map[string]struct{}, len(req.RelayIDs))
	byRelay := make(map[string]Lease, len(live))
	for _, lease := range live {
		byRelay[lease.RelayID] = lease
	}
	for _, relayID := range req.RelayIDs {
		requested[relayID] = struct{}{}
		if lease, ok := byRelay[relayID]; ok {
			plan.extend[relayID] = lease
		} else {
			plan.create = append(plan.create, relayID)
		}
	}
	affected := make(map[string]struct{}, len(req.RelayIDs))
	for relayID := range requested {
		affected[relayID] = struct{}{}
	}
	if over := len(live) + len(plan.create) - req.MaxPerUser; over > 0 {
		var candidates []Lease
		for _, lease := range live {
			if _, ok := requested[lease.RelayID]; !ok {
				candidates = append(candidates, lease)
			}
		}
		if over > len(candidates) {
			return leaseIssuePlan{}, ErrLeaseCapExceeded
		}
		// Least recently renewed first; the ID breaks ties deterministically.
		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].RenewedAt.Equal(candidates[j].RenewedAt) {
				return candidates[i].RenewedAt.Before(candidates[j].RenewedAt)
			}
			return candidates[i].ID < candidates[j].ID
		})
		for _, victim := range candidates[:over] {
			plan.victims[victim.RelayID] = append(plan.victims[victim.RelayID], victim)
			affected[victim.RelayID] = struct{}{}
		}
	}
	for relayID := range affected {
		plan.affected = append(plan.affected, relayID)
	}
	sort.Strings(plan.affected)
	return plan, nil
}

// leaseRelayAuthorizedLocked applies the shared guard: the relay must hold an
// identity-bearing registration whose lease token matches.
func (s *Store) leaseRelayAuthorizedLocked(relayID, leaseToken string) bool {
	desc, ok := s.relays[relayID]
	if !ok || desc.IdentityPublicKey == "" || desc.LeaseToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(desc.LeaseToken), []byte(leaseToken)) == 1
}

func (s *Store) RelayLeaseSet(_ context.Context, relayID, leaseToken string, now time.Time) (RelayLeaseSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.leaseRelayAuthorizedLocked(relayID, leaseToken) {
		return RelayLeaseSet{}, ErrRelayNotFound
	}
	var set RelayLeaseSet
	if state, ok := s.leaseStates[relayID]; ok {
		set.Revision, set.AppliedRevision = state.revision, state.applied
	}
	for _, lease := range s.leases {
		if lease.RelayID == relayID && lease.Live(now) {
			set.Leases = append(set.Leases, lease)
		}
	}
	sort.Slice(set.Leases, func(i, j int) bool { return set.Leases[i].ID < set.Leases[j].ID })
	return set, nil
}

func (s *Store) AckLeaseRevision(_ context.Context, relayID, leaseToken string, revision int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.leaseRelayAuthorizedLocked(relayID, leaseToken) {
		return ErrRelayNotFound
	}
	state := s.leaseStateLocked(relayID)
	if revision > state.applied {
		state.applied = revision
	}
	return nil
}

func (s *Store) AppliedLeaseRevisions(_ context.Context, relayIDs []string) (map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	applied := make(map[string]int64, len(relayIDs))
	for _, relayID := range relayIDs {
		if state, ok := s.leaseStates[relayID]; ok {
			applied[relayID] = state.applied
		} else {
			applied[relayID] = 0
		}
	}
	return applied, nil
}

func (s *Store) LeaseByID(_ context.Context, id string) (Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lease, ok := s.leases[id]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	return lease, nil
}

// revokeUserLeasesLocked revokes the user's live leases, one revision step per
// relay, and returns the affected relay IDs sorted.
func (s *Store) revokeUserLeasesLocked(userID string, now time.Time) []string {
	byRelay := make(map[string][]Lease)
	for _, lease := range s.liveLeasesForUserLocked(userID, now) {
		byRelay[lease.RelayID] = append(byRelay[lease.RelayID], lease)
	}
	relayIDs := make([]string, 0, len(byRelay))
	for relayID := range byRelay {
		relayIDs = append(relayIDs, relayID)
	}
	sort.Strings(relayIDs)
	for _, relayID := range relayIDs {
		state := s.leaseStateLocked(relayID)
		state.revision++
		for _, lease := range byRelay[relayID] {
			lease.RevokedAt = now
			lease.Revision = state.revision
			s.leases[lease.ID] = lease
		}
	}
	return relayIDs
}

// pruneLeasesLocked drops leases past their retention. Relay state rows are
// kept: a relay's revision must keep counting across its leases' lifetimes.
func (s *Store) pruneLeasesLocked(now time.Time) {
	for id, lease := range s.leases {
		if !lease.ExpiresAt.Add(leaseRetention).After(now) {
			delete(s.leases, id)
		}
	}
}
