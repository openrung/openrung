package broker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestPostgresStoreSharesRelayStateAcrossInstances(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	storeA := newTestPostgresStore(t, RankingModeGlobal)
	storeB := newTestPostgresStore(t, RankingModeGlobal)

	desc, err := storeA.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	listed, err := storeB.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list relays from second store: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != desc.ID {
		t.Fatalf("second store did not see registered relay: %+v", listed)
	}

	updated, err := storeB.Heartbeat(desc.ID, now.Add(30*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("heartbeat from second store: %v", err)
	}
	if !updated.ExpiresAt.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("unexpected heartbeat expiration: %s", updated.ExpiresAt)
	}
}

func TestPostgresStorePreservesActiveRelaysAcrossRestart(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStoreWithoutCleanup(t, RankingModeGlobal)
	cleanupPostgresStore(t, store)
	desc, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	store.Close()

	reopened := newTestPostgresStoreWithoutCleanup(t, RankingModeGlobal)
	t.Cleanup(func() { cleanupPostgresStore(t, reopened) })
	listed, err := reopened.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list relays after reopen: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != desc.ID {
		t.Fatalf("reopened store did not preserve relay: %+v", listed)
	}
}

func TestPostgresStorePruneIsShared(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	storeA := newTestPostgresStore(t, RankingModeGlobal)
	storeB := newTestPostgresStore(t, RankingModeGlobal)

	desc, err := storeA.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	expired, err := storeB.Prune(desc.ExpiresAt.Add(time.Nanosecond))
	if err != nil {
		t.Fatalf("prune from second store: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != desc.ID {
		t.Fatalf("unexpected expired relays: %+v", expired)
	}
	listed, err := storeA.List(desc.ExpiresAt.Add(time.Nanosecond), 10)
	if err != nil {
		t.Fatalf("list after shared prune: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected pruned relay to disappear from first store, got %+v", listed)
	}
}

func TestPostgresStoreDuplicateEndpointReplacesOldDescriptor(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	first, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register first relay: %v", err)
	}
	req := validRegisterRequest()
	req.ClientID = "1b5cceef-64f2-462b-b729-b89c3a63e6e2"
	second, err := store.Register(req, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("register replacement relay: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected replacement to receive a new relay ID")
	}
	if _, err := store.Heartbeat(first.ID, now.Add(2*time.Second), time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("expected old relay ID to be forgotten, got %v", err)
	}
	listed, err := store.List(now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list replacement relay: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != second.ID || listed[0].ClientID != req.ClientID {
		t.Fatalf("unexpected replacement list: %+v", listed)
	}
}

func TestPostgresStoreGlobalRankingUsesSharedMetrics(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	storeA := newTestPostgresStore(t, RankingModeGlobal)
	storeB := newTestPostgresStore(t, RankingModeGlobal)
	crowded := registerPostgresRelayForRanking(t, storeA, now, "crowded.example.com", 1, 20)
	open := registerPostgresRelayForRanking(t, storeA, now.Add(time.Second), "open.example.com", 8, 20)

	if err := storeB.RecordRelayTelemetry(context.Background(), []TelemetryRecord{{
		ReceivedAt: now.Add(10 * time.Second),
		Event: TelemetryEvent{
			EventID:    "postgres-heartbeat-1",
			Event:      "session_heartbeat",
			OccurredAt: now.Add(10 * time.Second),
			ClientID:   "client-1",
			SessionID:  "session-1",
			RelayID:    crowded.ID,
		},
	}}, now.Add(10*time.Second)); err != nil {
		t.Fatalf("record shared metrics: %v", err)
	}
	listed, err := storeA.List(now.Add(20*time.Second), 10)
	if err != nil {
		t.Fatalf("list ranked relays: %v", err)
	}
	if len(listed) < 2 || listed[0].ID != open.ID {
		t.Fatalf("expected open relay first from shared metrics, got %+v", listed)
	}
}

func newTestPostgresStore(t *testing.T, rankingMode RankingMode) *PostgresStore {
	t.Helper()
	store := newTestPostgresStoreWithoutCleanup(t, rankingMode)
	cleanupPostgresStore(t, store)
	t.Cleanup(func() {
		cleanupPostgresStore(t, store)
		store.Close()
	})
	return store
}

func newTestPostgresStoreWithoutCleanup(t *testing.T, rankingMode RankingMode) *PostgresStore {
	t.Helper()
	databaseURL := os.Getenv("OPENRUNG_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("OPENRUNG_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, databaseURL, rankingMode)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	return store
}

func cleanupPostgresStore(t *testing.T, store *PostgresStore) {
	t.Helper()
	if _, err := store.pool.Exec(context.Background(), `TRUNCATE relay_metrics, relay_sessions, relay_descriptors`); err != nil {
		t.Fatalf("cleanup postgres store: %v", err)
	}
}

func registerPostgresRelayForRanking(t *testing.T, store *PostgresStore, now time.Time, host string, maxSessions, maxMbps int) relay.Descriptor {
	t.Helper()
	req := validRegisterRequest()
	req.PublicHost = host
	req.MaxSessions = maxSessions
	req.MaxMbps = maxMbps
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	return desc
}
