// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Postgres userDirectory. The page is one statement in two steps: the page
// CTE filters, orders and limits users, then each kept row is joined
// laterally to an aggregate over its own access_leases (served by
// access_leases_by_user_idx), so the counters are computed for at most one
// page of users. Only the leases_24h ordering needs a figure before the page
// is chosen; it ranks through the grouped join r, which the planner
// removes under the time orderings because nothing there references it.

// userListOrders maps each UserSort to its ORDER BY clauses: one ranking the
// page CTE, one restoring the same order over the joined rows (l.issued is
// coalesce(r.issued, 0) by the same definition). The clauses are chosen from
// this fixed table, never built from input.
var userListOrders = map[string][2]string{
	UserSortLastAuth:  {`u.last_auth_at DESC, u.id DESC`, `u.last_auth_at DESC, u.id DESC`},
	UserSortLeases24h: {`coalesce(r.issued, 0) DESC, u.last_auth_at DESC, u.id DESC`, `l.issued DESC, u.last_auth_at DESC, u.id DESC`},
	UserSortEnrolled:  {`u.created_at DESC, u.id DESC`, `u.created_at DESC, u.id DESC`},
}

func (s *PostgresStore) ListUsers(parent context.Context, q UserListQuery, now time.Time) (UserListPage, error) {
	order, ok := userListOrders[q.Sort]
	if !ok {
		return UserListPage{}, fmt.Errorf("unknown user sort %q", q.Sort)
	}
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()

	page := UserListPage{Users: []UserListEntry{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM users
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR id = $2)`, q.Status, q.UserID).Scan(&page.Total); err != nil {
		return UserListPage{}, fmt.Errorf("count users: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		WITH page AS (
			SELECT u.id, u.status, u.created_at, u.last_auth_at, u.disabled_at, u.disabled_reason
			FROM users u
			LEFT JOIN (
				SELECT user_id, count(*) AS issued FROM access_leases
				WHERE created_at > $4
				GROUP BY user_id
			) r ON r.user_id = u.id
			WHERE ($1 = '' OR u.status = $1) AND ($2 = '' OR u.id = $2)
			ORDER BY `+order[0]+`
			LIMIT $5 OFFSET $6
		)
		SELECT u.id, u.status, u.created_at, u.last_auth_at, u.disabled_at, u.disabled_reason,
			l.live, l.issued, l.relays
		FROM page u
		CROSS JOIN LATERAL (
			SELECT
				count(*) FILTER (WHERE a.revoked_at IS NULL AND a.expires_at > $3) AS live,
				count(*) FILTER (WHERE a.created_at > $4) AS issued,
				count(DISTINCT a.relay_id) FILTER (WHERE a.renewed_at > $4) AS relays
			FROM access_leases a
			WHERE a.user_id = u.id
		) l
		ORDER BY `+order[1],
		q.Status, q.UserID, now, now.Add(-userActivityWindow), q.Limit, q.Offset)
	if err != nil {
		return UserListPage{}, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry UserListEntry
		var disabledAt sql.NullTime
		if err := rows.Scan(&entry.ID, &entry.Status, &entry.CreatedAt, &entry.LastAuthAt, &disabledAt, &entry.DisabledReason,
			&entry.LiveLeases, &entry.LeasesIssued24h, &entry.Relays24h); err != nil {
			return UserListPage{}, fmt.Errorf("scan user row: %w", err)
		}
		entry.CreatedAt = entry.CreatedAt.UTC()
		entry.LastAuthAt = entry.LastAuthAt.UTC()
		if disabledAt.Valid {
			entry.DisabledAt = disabledAt.Time.UTC()
		}
		page.Users = append(page.Users, entry)
	}
	if err := rows.Err(); err != nil {
		return UserListPage{}, fmt.Errorf("list users: %w", err)
	}
	return page, nil
}

func (s *PostgresStore) UserLeases(parent context.Context, userID string, limit int) ([]Lease, int, error) {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM access_leases WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count user leases: %w", err)
	}
	leases, err := scanLeases(s.pool.Query(ctx, `
		SELECT `+leaseColumns+` FROM access_leases
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, userID, limit))
	if err != nil {
		return nil, 0, fmt.Errorf("list user leases: %w", err)
	}
	return leases, total, nil
}
