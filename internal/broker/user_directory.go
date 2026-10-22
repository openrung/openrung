// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"openrung/internal/relay"
)

// Operator user directory: the read side behind the dashboard's Users page
// and GET /admin/api/users. A row is the user record without its public key
// plus three counters derived from access_leases — the broker keeps no
// per-user byte count, so lease churn is the only volume signal it has.
// Nothing here touches telemetry: user IDs are deliberately independent of
// telemetry client IDs and must stay unjoined.

const (
	userListDefaultLimit = 50
	userListMaxLimit     = 100
	// userLeaseHistoryLimit bounds the detail view's lease list to the most
	// recently issued leases.
	userLeaseHistoryLimit = 50
	// userActivityWindow is the look-back of the per-user lease counters.
	userActivityWindow = 24 * time.Hour
)

// User list orderings. Every one is descending with the user ID (descending)
// as the deterministic final tiebreak, identically on both stores.
const (
	// UserSortLastAuth orders by last_auth_at: the default.
	UserSortLastAuth = "last_auth"
	// UserSortLeases24h orders by leases issued in the activity window, then
	// by last_auth_at.
	UserSortLeases24h = "leases_24h"
	// UserSortEnrolled orders by created_at, then by user ID: newest
	// enrollments first.
	UserSortEnrolled = "enrolled"
)

func validUserSort(sort string) bool {
	return sort == UserSortLastAuth || sort == UserSortLeases24h || sort == UserSortEnrolled
}

// UserListQuery selects one page of the directory.
type UserListQuery struct {
	// Status is "" for every user, or one of the two user states.
	Status string
	// Sort is one of the UserSort constants.
	Sort string
	// UserID narrows the result to one user ("" for all): the detail view
	// reads its summary row through the same query as the table.
	UserID string
	Offset int
	Limit  int
}

// UserListEntry is one directory row. It deliberately has no public key.
type UserListEntry struct {
	ID             string
	Status         string
	CreatedAt      time.Time
	LastAuthAt     time.Time
	DisabledAt     time.Time
	DisabledReason string
	// LiveLeases counts leases neither revoked nor expired at now.
	LiveLeases int
	// LeasesIssued24h counts leases created in the activity window.
	LeasesIssued24h int
	// Relays24h counts distinct relays among leases issued or renewed in
	// the activity window.
	Relays24h int
}

// UserListPage is one page plus the number of users matching the filter.
type UserListPage struct {
	Total int
	Users []UserListEntry
}

// userDirectory is the slice of the access store the users page and the
// operational list read. Narrow so the dashboard can take it without the
// whole AccessStore, following relayRankingWeightStore.
type userDirectory interface {
	// ListUsers returns the users matching q in q.Sort order, paged by
	// q.Offset and q.Limit, with the lease counters evaluated at now.
	ListUsers(ctx context.Context, q UserListQuery, now time.Time) (UserListPage, error)
	// UserLeases returns the user's most recently created retained leases
	// (created_at, then ID, descending), at most limit, and how many are
	// retained in all.
	UserLeases(ctx context.Context, userID string, limit int) ([]Lease, int, error)
	// LeaseByID resolves a lease label to its owner (see LeaseStore).
	LeaseByID(ctx context.Context, id string) (Lease, error)
}

var (
	_ userDirectory = (*Store)(nil)
	_ userDirectory = (*PostgresStore)(nil)
)

// resolveUserRef maps the users page's search input to a user ID: a user ID
// as is, or a lease label as relays log it ("lease-<id>") or a bare lease ID
// to the lease's owner, returned with the lease ID. Anything else, and any
// lease past its retention, is ErrUserNotFound.
func resolveUserRef(ctx context.Context, directory userDirectory, ref string) (userID, leaseID string, err error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if wellFormedUserID(ref) {
		return ref, "", nil
	}
	id := strings.TrimPrefix(ref, relay.LeaseEmailPrefix)
	if !wellFormedLeaseID(id) {
		return "", "", ErrUserNotFound
	}
	lease, err := directory.LeaseByID(ctx, id)
	if errors.Is(err, ErrLeaseNotFound) {
		return "", "", ErrUserNotFound
	}
	if err != nil {
		return "", "", err
	}
	return lease.UserID, lease.ID, nil
}

// In-memory userDirectory. It mirrors the Postgres query: the same counter
// definitions, the same orderings, the same paging.

func (s *Store) ListUsers(_ context.Context, q UserListQuery, now time.Time) (UserListPage, error) {
	since := now.Add(-userActivityWindow)
	s.mu.RLock()
	defer s.mu.RUnlock()

	type counters struct {
		live, issued int
		relays       map[string]struct{}
	}
	byUser := make(map[string]*counters)
	for _, lease := range s.leases {
		if q.UserID != "" && lease.UserID != q.UserID {
			continue
		}
		c := byUser[lease.UserID]
		if c == nil {
			c = &counters{relays: make(map[string]struct{})}
			byUser[lease.UserID] = c
		}
		if lease.Live(now) {
			c.live++
		}
		if lease.CreatedAt.After(since) {
			c.issued++
		}
		if lease.RenewedAt.After(since) {
			c.relays[lease.RelayID] = struct{}{}
		}
	}

	var entries []UserListEntry
	for _, user := range s.users {
		if (q.Status != "" && user.Status != q.Status) || (q.UserID != "" && user.ID != q.UserID) {
			continue
		}
		entry := UserListEntry{
			ID: user.ID, Status: user.Status, CreatedAt: user.CreatedAt, LastAuthAt: user.LastAuthAt,
			DisabledAt: user.DisabledAt, DisabledReason: user.DisabledReason,
		}
		if c := byUser[user.ID]; c != nil {
			entry.LiveLeases, entry.LeasesIssued24h, entry.Relays24h = c.live, c.issued, len(c.relays)
		}
		entries = append(entries, entry)
	}
	sortUserListEntries(entries, q.Sort)

	page := UserListPage{Total: len(entries), Users: []UserListEntry{}}
	if q.Offset < len(entries) {
		end := min(len(entries), q.Offset+q.Limit)
		page.Users = append(page.Users, entries[q.Offset:end]...)
	}
	return page, nil
}

// sortUserListEntries applies a UserSort ordering (see the constants).
func sortUserListEntries(entries []UserListEntry, by string) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch by {
		case UserSortLeases24h:
			if a.LeasesIssued24h != b.LeasesIssued24h {
				return a.LeasesIssued24h > b.LeasesIssued24h
			}
		case UserSortEnrolled:
			// Enrollment never consults last_auth_at, so re-authentication
			// cannot reorder it.
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return a.ID > b.ID
		}
		if !a.LastAuthAt.Equal(b.LastAuthAt) {
			return a.LastAuthAt.After(b.LastAuthAt)
		}
		return a.ID > b.ID
	})
}

func (s *Store) UserLeases(_ context.Context, userID string, limit int) ([]Lease, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var leases []Lease
	for _, lease := range s.leases {
		if lease.UserID == userID {
			leases = append(leases, lease)
		}
	}
	sort.Slice(leases, func(i, j int) bool {
		if !leases[i].CreatedAt.Equal(leases[j].CreatedAt) {
			return leases[i].CreatedAt.After(leases[j].CreatedAt)
		}
		return leases[i].ID > leases[j].ID
	})
	total := len(leases)
	if len(leases) > limit {
		leases = leases[:limit]
	}
	return leases, total, nil
}
