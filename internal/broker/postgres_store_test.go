package broker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"openrung/internal/relay"
)

type heartbeatMissRow func(...any) error

func (row heartbeatMissRow) Scan(dest ...any) error {
	return row(dest...)
}

func TestHeartbeatMissError(t *testing.T) {
	lookupErr := errors.New("lookup failed")
	tests := []struct {
		name string
		row  heartbeatMissRow
		want error
	}{
		{
			name: "foundation row exists",
			row: func(dest ...any) error {
				*dest[0].(*bool) = true
				return nil
			},
			want: ErrNodeClassForbidden,
		},
		{
			name: "relay is missing",
			row: func(dest ...any) error {
				*dest[0].(*bool) = false
				return nil
			},
			want: ErrRelayNotFound,
		},
		{
			name: "lookup fails",
			row:  func(...any) error { return lookupErr },
			want: lookupErr,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := heartbeatMissError(test.row); !errors.Is(err, test.want) {
				t.Fatalf("heartbeat miss error = %v, want %v", err, test.want)
			}
		})
	}
}

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

	updated, err := storeB.Heartbeat(desc.ID, relay.NodeClassVolunteer, now.Add(30*time.Second), time.Minute)
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
	if _, err := store.Heartbeat(first.ID, relay.NodeClassVolunteer, now.Add(2*time.Second), time.Minute); !errors.Is(err, ErrRelayNotFound) {
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

func TestPostgresStoreGeoSurvivesReRegistration(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	desc, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	geo := relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP"}
	if err := store.UpdateGeo(desc.ID, geo); err != nil {
		t.Fatalf("update geo: %v", err)
	}
	listed, err := store.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(listed) != 1 || listed[0].GeoLocation != geo {
		t.Fatalf("expected listed relay to carry geo, got %+v", listed)
	}

	// Re-registering the same host:port must keep the resolved location.
	replacement, err := store.Register(validRegisterRequest(), now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("re-register relay: %v", err)
	}
	if replacement.GeoLocation != geo {
		t.Fatalf("expected re-registered relay to keep geo, got %+v", replacement.GeoLocation)
	}

	if err := store.UpdateGeo("relay_missing", geo); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("expected ErrRelayNotFound for unknown relay, got %v", err)
	}
}

func TestPostgresStoreExitHostChangeClearsGeo(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	req := validRegisterRequest()
	req.Transport = relay.TransportTunnel
	req.ExitHost = "198.51.100.7"
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register tunnel relay: %v", err)
	}
	if desc.ExitHost != "198.51.100.7" {
		t.Fatalf("expected stored exit host, got %q", desc.ExitHost)
	}
	geo := relay.GeoLocation{City: "Tehran", Country: "Iran", CountryCode: "IR"}
	if err := store.UpdateGeo(desc.ID, geo); err != nil {
		t.Fatalf("update geo: %v", err)
	}

	// Same hub endpoint, same exit host: the location sticks.
	sameExit, err := store.Register(req, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("re-register same exit: %v", err)
	}
	if sameExit.GeoLocation != geo {
		t.Fatalf("expected geo to survive same-exit re-registration, got %+v", sameExit.GeoLocation)
	}

	// Same hub endpoint reused by a different volunteer: stale location cleared.
	req.ExitHost = "198.51.100.99"
	newExit, err := store.Register(req, now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("re-register new exit: %v", err)
	}
	if newExit.GeoLocation != (relay.GeoLocation{}) {
		t.Fatalf("expected geo cleared after exit host change, got %+v", newExit.GeoLocation)
	}
	if newExit.ExitHost != "198.51.100.99" {
		t.Fatalf("expected updated exit host, got %q", newExit.ExitHost)
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

func TestPostgresStoreRoundTripsNodeClass(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("registered node_class = %q, want %q", desc.NodeClass, relay.NodeClassFoundation)
	}

	listed, err := store.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(listed) != 1 || listed[0].NodeClass != relay.NodeClassFoundation {
		t.Fatalf("listed node_class not preserved: %+v", listed)
	}

	// A volunteer-class registration at a DIFFERENT endpoint round-trips as
	// volunteer. (A volunteer registration at the SAME endpoint is refused by
	// the foundation guard; see TestPostgresStoreRegisterGuardsFoundationEndpoint.)
	vol := validRegisterRequest()
	vol.PublicHost = "2001:db8::abcd"
	volDesc, err := store.Register(vol, now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("register volunteer relay: %v", err)
	}
	if volDesc.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("volunteer node_class = %q, want %q", volDesc.NodeClass, relay.NodeClassVolunteer)
	}
}

func TestPostgresStoreHeartbeatGuardsFoundationLease(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}

	if _, err := store.Heartbeat(desc.ID, relay.NodeClassVolunteer, now.Add(time.Second), time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
		t.Fatalf("volunteer-credential heartbeat of foundation relay: err = %v, want ErrNodeClassForbidden", err)
	}
	// The refused heartbeat must not have extended the lease: past the
	// original TTL the relay is gone.
	if listed, err := store.List(now.Add(2*time.Minute), 10); err != nil || len(listed) != 0 {
		t.Fatalf("foundation relay lease was extended by refused heartbeat: %+v %v", listed, err)
	}

	updated, err := store.Heartbeat(desc.ID, relay.NodeClassFoundation, now.Add(30*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("foundation-credential heartbeat: %v", err)
	}
	if updated.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("heartbeat descriptor node_class = %q, want %q", updated.NodeClass, relay.NodeClassFoundation)
	}

	if _, err := store.Heartbeat("relay_missing", relay.NodeClassFoundation, now, time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("missing relay: err = %v, want ErrRelayNotFound", err)
	}
}

func TestPostgresStoreRegisterGuardsFoundationEndpoint(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	foundation := validRegisterRequest()
	foundation.NodeClass = relay.NodeClassFoundation
	foundation.RealityPublicKey = "foundation-key"
	original, err := store.Register(foundation, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}

	// Anonymous/volunteer registration at the same host:port must be refused,
	// not overwrite the foundation descriptor.
	attacker := validRegisterRequest() // same public_host:public_port
	attacker.RealityPublicKey = "attacker-key"
	if _, err := store.Register(attacker, now.Add(time.Second), time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
		t.Fatalf("attacker overwrite: err = %v, want ErrNodeClassForbidden", err)
	}

	listed, err := store.List(now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected exactly the foundation relay, got %d: %+v", len(listed), listed)
	}
	got := listed[0]
	if got.ID != original.ID || got.NodeClass != relay.NodeClassFoundation || got.RealityPublicKey != "foundation-key" {
		t.Fatalf("foundation descriptor was disturbed: %+v", got)
	}

	// The foundation operator can still refresh its own endpoint with a
	// foundation-class registration.
	refreshed, err := store.Register(foundation, now.Add(3*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("foundation refresh: %v", err)
	}
	if refreshed.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("refresh node_class = %q, want foundation", refreshed.NodeClass)
	}
}

// An expired-but-unpruned foundation row must not block a fresh registration
// at the same endpoint: the postgres guard checks liveness, matching the
// in-memory store (parity for the "live foundation endpoint" contract).
func TestPostgresStoreExpiredFoundationEndpointIsReclaimable(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := newTestPostgresStore(t, RankingModeGlobal)

	foundation := validRegisterRequest()
	foundation.NodeClass = relay.NodeClassFoundation
	if _, err := store.Register(foundation, now, time.Minute); err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}

	// A live foundation row still blocks a volunteer takeover.
	vol := validRegisterRequest()
	if _, err := store.Register(vol, now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
		t.Fatalf("live foundation endpoint: err = %v, want ErrNodeClassForbidden", err)
	}

	// Past the foundation lease (but before any prune), the same volunteer
	// registration succeeds and reclaims the endpoint as volunteer.
	reclaimed, err := store.Register(vol, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("reclaim expired foundation endpoint: %v", err)
	}
	if reclaimed.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("reclaimed node_class = %q, want %q", reclaimed.NodeClass, relay.NodeClassVolunteer)
	}
}
