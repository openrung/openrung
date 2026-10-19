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

// Postgres AccessStore. Schema lives in postgresSchema (postgres_store.go);
// the user row is the durable identity, the enrollment counters are bounded
// accounting that Prune sweeps once their window ends.

const userColumns = `id, public_key, status, created_at, last_auth_at, disabled_at, disabled_reason`

func (s *PostgresStore) AuthenticateUser(parent context.Context, canonicalPublicKey, sourceKey string, windows []EnrollmentWindow, refuseNew error, now time.Time) (User, bool, error) {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, false, fmt.Errorf("begin user authentication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// An existing key renews its last_auth_at and is never charged.
	user, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users SET last_auth_at = $2 WHERE public_key = $1
		RETURNING `+userColumns, canonicalPublicKey, now))
	if err == nil {
		return user, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, err
	}
	if refuseNew != nil {
		// Nothing was written; the deferred rollback ends the transaction.
		return User{}, false, refuseNew
	}

	// Create the user first, then charge the budget, all in this transaction.
	// The unique index on public_key serialises concurrent first
	// authentications of one key: the loser blocks until the winner commits,
	// then its DO NOTHING returns no row and it takes the existing-user path
	// below without ever reaching the counters — so exactly the transaction
	// that creates a user pays for it. If the winner instead rolls back on an
	// exhausted budget, the loser proceeds as the creator. Exceeding any
	// window rolls the whole transaction back, user row and charges alike,
	// so a refused attempt leaves everything as it was.
	id, err := newUserID()
	if err != nil {
		return User{}, false, err
	}
	user, err = scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (id, public_key, status, created_at, last_auth_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (public_key) DO NOTHING
		RETURNING `+userColumns, id, canonicalPublicKey, brokerapi.UserStatusEnabled, now))
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET last_auth_at = $2 WHERE public_key = $1
			RETURNING `+userColumns, canonicalPublicKey, now))
		if err != nil {
			return User{}, false, fmt.Errorf("adopt concurrently created user: %w", err)
		}
		return user, false, tx.Commit(ctx)
	}
	if err != nil {
		return User{}, false, fmt.Errorf("store user: %w", err)
	}

	var retryAfter time.Duration
	for _, w := range windows {
		var count int
		var windowStart time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO user_enrollment_counters (source_key, window_name, window_start, window_seconds, count)
			VALUES ($1, $2, $3, $4, 1)
			ON CONFLICT (source_key, window_name) DO UPDATE SET
				count = CASE
					WHEN user_enrollment_counters.window_start + user_enrollment_counters.window_seconds * interval '1 second' <= EXCLUDED.window_start
					THEN 1 ELSE user_enrollment_counters.count + 1 END,
				window_start = CASE
					WHEN user_enrollment_counters.window_start + user_enrollment_counters.window_seconds * interval '1 second' <= EXCLUDED.window_start
					THEN EXCLUDED.window_start ELSE user_enrollment_counters.window_start END,
				window_seconds = EXCLUDED.window_seconds
			RETURNING count, window_start`,
			sourceKey, w.Name, now, int64(w.Window/time.Second),
		).Scan(&count, &windowStart)
		if err != nil {
			return User{}, false, fmt.Errorf("charge enrollment budget: %w", err)
		}
		if count > w.Limit {
			wait := windowStart.Add(w.Window).Sub(now)
			if retryAfter == 0 || wait < retryAfter {
				retryAfter = wait
			}
		}
	}
	if retryAfter > 0 {
		// Rolls back the user row inserted above together with the charges.
		return User{}, false, &EnrollmentLimitedError{RetryAfter: retryAfter}
	}
	return user, true, tx.Commit(ctx)
}

func (s *PostgresStore) UserByID(parent context.Context, id string) (User, error) {
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	user, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return user, err
}

func (s *PostgresStore) SetUserStatus(parent context.Context, id, status, reason string, now time.Time) (User, []string, error) {
	if !validUserStatus(status) {
		return User{}, nil, ErrInvalidUserStatus
	}
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, nil, fmt.Errorf("begin user status update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	disabling := status == brokerapi.UserStatusDisabled
	// The UPDATE takes the user's row lock — the same lock lease issuance
	// takes first — so a disable and an issuance for one user serialise and
	// the revocation below cannot miss a lease issued concurrently.
	user, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users
		SET status = $2,
			disabled_at = CASE WHEN $3 THEN $4::timestamptz ELSE NULL END,
			disabled_reason = CASE WHEN $3 THEN $5 ELSE '' END
		WHERE id = $1
		RETURNING `+userColumns, id, status, disabling, now, reason))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, nil, ErrUserNotFound
	}
	if err != nil {
		return User{}, nil, err
	}
	var changed []string
	if disabling {
		if changed, err = revokeUserLeases(ctx, tx, id, now); err != nil {
			return User{}, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, nil, fmt.Errorf("commit user status update: %w", err)
	}
	return user, changed, nil
}

// pruneEnrollmentCounters drops counters whose window has ended.
func (s *PostgresStore) pruneEnrollmentCounters(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM user_enrollment_counters
		WHERE window_start + window_seconds * interval '1 second' <= $1`, now)
	return err
}

func scanUser(row pgx.Row) (User, error) {
	var user User
	var disabledAt sql.NullTime
	if err := row.Scan(&user.ID, &user.PublicKey, &user.Status, &user.CreatedAt, &user.LastAuthAt, &disabledAt, &user.DisabledReason); err != nil {
		return User{}, err
	}
	user.CreatedAt = user.CreatedAt.UTC()
	user.LastAuthAt = user.LastAuthAt.UTC()
	if disabledAt.Valid {
		user.DisabledAt = disabledAt.Time.UTC()
	}
	return user, nil
}
