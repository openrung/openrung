// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openrung/openrung/brokerapi"
)

// Postgres LeaseStore. Schema lives in postgresSchema (postgres_store.go).
// Locking follows the contract in leases.go: user row first, then the
// affected relay_lease_state rows in sorted ID order, every change to a
// relay's set in one transaction that advances that relay's revision.

var _ AccessStore = (*PostgresStore)(nil)
var _ AccessStore = (*Store)(nil)

const leaseColumns = `id, user_id, relay_id, credential, created_at, renewed_at, expires_at, revoked_at, revision`

func (s *PostgresStore) IssueLeases(parent context.Context, req LeaseIssueRequest, now time.Time) (LeaseIssueResult, error) {
	req = req.normalized()
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LeaseIssueResult{}, fmt.Errorf("begin lease issuance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The user row lock serialises every multi-relay write for this user, so
	// two concurrent requests cannot both see "no live lease here" and insert
	// two leases on one relay.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, req.UserID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseIssueResult{}, ErrUserNotFound
	}
	if err != nil {
		return LeaseIssueResult{}, fmt.Errorf("lock user: %w", err)
	}
	if status != brokerapi.UserStatusEnabled {
		return LeaseIssueResult{}, ErrUserDisabled
	}

	live, err := scanLeases(tx.Query(ctx, `
		SELECT `+leaseColumns+` FROM access_leases
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY id`, req.UserID, now))
	if err != nil {
		return LeaseIssueResult{}, fmt.Errorf("read live leases: %w", err)
	}
	plan, err := planLeaseIssue(req, live)
	if err != nil {
		return LeaseIssueResult{}, err
	}
	revisions, err := lockLeaseStates(ctx, tx, plan.affected, now)
	if err != nil {
		return LeaseIssueResult{}, err
	}

	// With every affected relay locked, capacity is exact; it decides which
	// creates are issuable, and only those count against the user's cap.
	eligible := make(map[string]bool, len(plan.create))
	for _, relayID := range plan.create {
		var count int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM access_leases
			WHERE relay_id = $1 AND revoked_at IS NULL AND expires_at > $2`, relayID, now).Scan(&count); err != nil {
			return LeaseIssueResult{}, fmt.Errorf("count relay leases: %w", err)
		}
		eligible[relayID] = count < req.MaxPerRelay
	}
	victimsByRelay := plan.victims(eligible)

	var result LeaseIssueResult
	expiresAt := now.Add(req.TTL)
	for _, relayID := range plan.affected {
		revision := revisions[relayID] + 1
		changed := false
		if victims := victimsByRelay[relayID]; len(victims) > 0 {
			ids := make([]string, 0, len(victims))
			for _, victim := range victims {
				ids = append(ids, victim.ID)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE access_leases SET revoked_at = $2, revision = $3
				WHERE id = ANY($1) AND revoked_at IS NULL`, ids, now, revision); err != nil {
				return LeaseIssueResult{}, fmt.Errorf("evict leases: %w", err)
			}
			changed = true
		}
		if extend, ok := plan.extend[relayID]; ok {
			lease, err := scanLease(tx.QueryRow(ctx, `
				UPDATE access_leases SET renewed_at = $2, expires_at = $3, revision = $4
				WHERE id = $1 RETURNING `+leaseColumns, extend.ID, now, expiresAt, revision))
			if err != nil {
				return LeaseIssueResult{}, fmt.Errorf("extend lease: %w", err)
			}
			result.Leases = append(result.Leases, lease)
			changed = true
		}
		if eligible[relayID] {
			id, err := newLeaseID()
			if err != nil {
				return LeaseIssueResult{}, err
			}
			credential, err := newLeaseCredential()
			if err != nil {
				return LeaseIssueResult{}, err
			}
			lease, err := scanLease(tx.QueryRow(ctx, `
				INSERT INTO access_leases (`+leaseColumns+`)
				VALUES ($1, $2, $3, $4, $5, $5, $6, NULL, $7)
				RETURNING `+leaseColumns, id, req.UserID, relayID, credential, now, expiresAt, revision))
			if err != nil {
				return LeaseIssueResult{}, fmt.Errorf("insert lease: %w", err)
			}
			result.Leases = append(result.Leases, lease)
			changed = true
		}
		if changed {
			if _, err := tx.Exec(ctx, `
				UPDATE relay_lease_state SET revision = $2, updated_at = $3 WHERE relay_id = $1`,
				relayID, revision, now); err != nil {
				return LeaseIssueResult{}, fmt.Errorf("advance relay revision: %w", err)
			}
			result.ChangedRelayIDs = append(result.ChangedRelayIDs, relayID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return LeaseIssueResult{}, fmt.Errorf("commit lease issuance: %w", err)
	}
	return result, nil
}

// lockLeaseStates takes FOR UPDATE on the state rows of relayIDs in the given
// (sorted) order, creating missing rows at revision 0 first, and returns each
// relay's current revision. Every multi-relay writer locks in this order, so
// overlapping writers queue instead of deadlocking.
func lockLeaseStates(ctx context.Context, tx pgx.Tx, relayIDs []string, now time.Time) (map[string]int64, error) {
	revisions := make(map[string]int64, len(relayIDs))
	for _, relayID := range relayIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO relay_lease_state (relay_id, revision, applied_revision, updated_at)
			VALUES ($1, 0, 0, $2) ON CONFLICT (relay_id) DO NOTHING`, relayID, now); err != nil {
			return nil, fmt.Errorf("create relay lease state: %w", err)
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM relay_lease_state WHERE relay_id = $1 FOR UPDATE`, relayID).Scan(&revision); err != nil {
			return nil, fmt.Errorf("lock relay lease state: %w", err)
		}
		revisions[relayID] = revision
	}
	return revisions, nil
}

func (s *PostgresStore) RelayLeaseSet(parent context.Context, relayID, leaseToken string, now time.Time) (RelayLeaseSet, error) {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	// One REPEATABLE READ snapshot covers the guard, the revision and the
	// leases, so a set served at revision N contains exactly the leases
	// stamped ≤ N — the property every ack relies on.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RelayLeaseSet{}, fmt.Errorf("begin relay lease read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var set RelayLeaseSet
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(s.revision, 0), COALESCE(s.applied_revision, 0)
		FROM relay_descriptors d
		LEFT JOIN relay_lease_state s ON s.relay_id = d.id
		WHERE d.id = $1 AND d.identity_public_key <> '' AND d.lease_token <> '' AND d.lease_token = $2`,
		relayID, leaseToken).Scan(&set.Revision, &set.AppliedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayLeaseSet{}, ErrRelayNotFound
	}
	if err != nil {
		return RelayLeaseSet{}, fmt.Errorf("read relay lease state: %w", err)
	}
	set.Leases, err = scanLeases(tx.Query(ctx, `
		SELECT `+leaseColumns+` FROM access_leases
		WHERE relay_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY id`, relayID, now))
	if err != nil {
		return RelayLeaseSet{}, fmt.Errorf("read relay leases: %w", err)
	}
	return set, tx.Commit(ctx)
}

func (s *PostgresStore) AckLeaseRevision(parent context.Context, relayID, leaseToken string, revision int64, now time.Time) error {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lease ack: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// FOR SHARE on the descriptor row orders this ack against a concurrent
	// re-registration: the registration's upsert waits for the share lock,
	// or the guard re-evaluates against the new row and misses. Either way a
	// stale ack can never land after the reset it would undo.
	var one int
	err = tx.QueryRow(ctx, `
		SELECT 1 FROM relay_descriptors
		WHERE id = $1 AND identity_public_key <> '' AND lease_token <> '' AND lease_token = $2
		FOR SHARE`, relayID, leaseToken).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRelayNotFound
	}
	if err != nil {
		return fmt.Errorf("check relay lease token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO relay_lease_state (relay_id, revision, applied_revision, updated_at)
		VALUES ($1, 0, $2, $3)
		ON CONFLICT (relay_id) DO UPDATE SET
			applied_revision = GREATEST(relay_lease_state.applied_revision, EXCLUDED.applied_revision),
			updated_at = EXCLUDED.updated_at`, relayID, revision, now); err != nil {
		return fmt.Errorf("record lease ack: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) AppliedLeaseRevisions(parent context.Context, relayIDs []string) (map[string]int64, error) {
	applied := make(map[string]int64, len(relayIDs))
	for _, relayID := range relayIDs {
		applied[relayID] = 0
	}
	if len(relayIDs) == 0 {
		return applied, nil
	}
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT relay_id, applied_revision FROM relay_lease_state WHERE relay_id = ANY($1)`, relayIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var relayID string
		var revision int64
		if err := rows.Scan(&relayID, &revision); err != nil {
			return nil, err
		}
		applied[relayID] = revision
	}
	return applied, rows.Err()
}

func (s *PostgresStore) LeaseByID(parent context.Context, id string) (Lease, error) {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	lease, err := scanLease(s.pool.QueryRow(ctx, `SELECT `+leaseColumns+` FROM access_leases WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseNotFound
	}
	return lease, err
}

// revokeUserLeases revokes the user's live leases inside tx, one revision
// step per relay in sorted order, and returns the affected relay IDs. The
// caller must already hold the user's row lock.
func revokeUserLeases(ctx context.Context, tx pgx.Tx, userID string, now time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT relay_id FROM access_leases
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY relay_id`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list user relays: %w", err)
	}
	var relayIDs []string
	for rows.Next() {
		var relayID string
		if err := rows.Scan(&relayID); err != nil {
			rows.Close()
			return nil, err
		}
		relayIDs = append(relayIDs, relayID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	revisions, err := lockLeaseStates(ctx, tx, relayIDs, now)
	if err != nil {
		return nil, err
	}
	for _, relayID := range relayIDs {
		revision := revisions[relayID] + 1
		if _, err := tx.Exec(ctx, `
			UPDATE access_leases SET revoked_at = $3, revision = $4
			WHERE user_id = $1 AND relay_id = $2 AND revoked_at IS NULL AND expires_at > $3`,
			userID, relayID, now, revision); err != nil {
			return nil, fmt.Errorf("revoke leases: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE relay_lease_state SET revision = $2, updated_at = $3 WHERE relay_id = $1`,
			relayID, revision, now); err != nil {
			return nil, fmt.Errorf("advance relay revision: %w", err)
		}
	}
	return relayIDs, nil
}

// pruneLeases drops leases past their retention; relay_lease_state is kept.
func (s *PostgresStore) pruneLeases(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM access_leases WHERE expires_at <= $1`, now.Add(-leaseRetention))
	return err
}

func scanLease(row pgx.Row) (Lease, error) {
	var lease Lease
	var revokedAt sql.NullTime
	if err := row.Scan(&lease.ID, &lease.UserID, &lease.RelayID, &lease.Credential, &lease.CreatedAt, &lease.RenewedAt, &lease.ExpiresAt, &revokedAt, &lease.Revision); err != nil {
		return Lease{}, err
	}
	lease.CreatedAt = lease.CreatedAt.UTC()
	lease.RenewedAt = lease.RenewedAt.UTC()
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	if revokedAt.Valid {
		lease.RevokedAt = revokedAt.Time.UTC()
	}
	return lease, nil
}

func scanLeases(rows pgx.Rows, err error) ([]Lease, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leases []Lease
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}
