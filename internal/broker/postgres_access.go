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

func (s *PostgresStore) AuthenticateUser(parent context.Context, canonicalPublicKey, sourceKey string, windows []EnrollmentWindow, now time.Time) (User, bool, error) {
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

	// A new identity is charged against every window inside this transaction.
	// The upsert resets a window that has ended and otherwise increments it;
	// exceeding any limit rolls the whole transaction back, so a refused
	// attempt leaves every counter as it was.
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
		return User{}, false, &EnrollmentLimitedError{RetryAfter: retryAfter}
	}

	id, err := newUserID()
	if err != nil {
		return User{}, false, err
	}
	user, err = scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (id, public_key, status, created_at, last_auth_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (public_key) DO NOTHING
		RETURNING `+userColumns, id, canonicalPublicKey, brokerapi.UserStatusEnabled, now))
	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent first authentication of the same key committed between
		// our UPDATE and INSERT: adopt its row. The budget charge above stands
		// for this rare race; it is one unit, not an unbounded leak.
		created = false
		user, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET last_auth_at = $2 WHERE public_key = $1
			RETURNING `+userColumns, canonicalPublicKey, now))
	}
	if err != nil {
		return User{}, false, fmt.Errorf("store user: %w", err)
	}
	return user, created, tx.Commit(ctx)
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

func (s *PostgresStore) SetUserStatus(parent context.Context, id, status, reason string, now time.Time) (User, error) {
	if !validUserStatus(status) {
		return User{}, ErrInvalidUserStatus
	}
	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	disabling := status == brokerapi.UserStatusDisabled
	user, err := scanUser(s.pool.QueryRow(ctx, `
		UPDATE users
		SET status = $2,
			disabled_at = CASE WHEN $3 THEN $4::timestamptz ELSE NULL END,
			disabled_reason = CASE WHEN $3 THEN $5 ELSE '' END
		WHERE id = $1
		RETURNING `+userColumns, id, status, disabling, now, reason))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return user, err
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
