package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestStorePrunesExpiredRelays(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	desc, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}

	if got, err := store.List(now.Add(30*time.Second), 10); err != nil || len(got) != 1 {
		if err != nil {
			t.Fatalf("list relays: %v", err)
		}
		t.Fatalf("expected relay before expiration, got %d", len(got))
	}

	if got, err := store.List(desc.ExpiresAt.Add(time.Nanosecond), 10); err != nil || len(got) != 0 {
		if err != nil {
			t.Fatalf("list relays: %v", err)
		}
		t.Fatalf("expected relay to be pruned after expiration, got %d", len(got))
	}
}

func TestStoreStatsAndPruneReportOnlyActiveRelays(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	expiring, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register expiring relay: %v", err)
	}
	activeReq := validRegisterRequest()
	activeReq.MaxSessions = 12
	if _, err := store.Register(activeReq, now, 10*time.Minute); err != nil {
		t.Fatalf("register active relay: %v", err)
	}

	checkAt := now.Add(2 * time.Minute)
	stats, err := store.Stats(checkAt)
	if err != nil {
		t.Fatalf("store stats: %v", err)
	}
	if stats.ActiveVolunteers != 1 || stats.AdvertisedSessionCapacity != 12 {
		t.Fatalf("unexpected store stats: %+v", stats)
	}
	expired, err := store.Prune(checkAt)
	if err != nil {
		t.Fatalf("prune relays: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != expiring.ID {
		t.Fatalf("unexpected expired relays: %+v", expired)
	}
	if expiredAgain, err := store.Prune(checkAt); err != nil || len(expiredAgain) != 0 {
		if err != nil {
			t.Fatalf("prune relays again: %v", err)
		}
		t.Fatalf("expected expiration to be reported once, got %+v", expiredAgain)
	}
}

func TestHeartbeatExtendsRelayLease(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	desc, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}

	heartbeatAt := now.Add(30 * time.Second)
	updated, err := store.Heartbeat(desc.ID, relay.NodeClassVolunteer, heartbeatAt, time.Minute)
	if err != nil {
		t.Fatalf("heartbeat relay: %v", err)
	}

	if !updated.ExpiresAt.Equal(heartbeatAt.Add(time.Minute)) {
		t.Fatalf("expected expiration %s, got %s", heartbeatAt.Add(time.Minute), updated.ExpiresAt)
	}
}

func TestStoreUpdateGeo(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	desc, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	if desc.GeoLocation != (relay.GeoLocation{}) {
		t.Fatalf("expected freshly registered relay without geo, got %+v", desc.GeoLocation)
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

	if err := store.UpdateGeo("relay_missing", geo); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("expected ErrRelayNotFound for unknown relay, got %v", err)
	}
}

func TestStoreGlobalRankingUsesFreshHeartbeatBeforeIPv6TieBreak(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	ipv6Req := validRegisterRequest()
	ipv6Req.PublicHost = "2001:db8::443"
	if _, err := store.Register(ipv6Req, now, time.Minute); err != nil {
		t.Fatalf("register ipv6 relay: %v", err)
	}

	ipv4Req := validRegisterRequest()
	ipv4Req.PublicHost = "203.0.113.10"
	if _, err := store.Register(ipv4Req, now.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("register ipv4 relay: %v", err)
	}

	got, err := store.List(now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 relays, got %d", len(got))
	}
	if got[0].PublicHost != "203.0.113.10" {
		t.Fatalf("expected newest heartbeat first when scores are neutral, got %q", got[0].PublicHost)
	}
}

func TestStoreLegacyRankingListsIPv6RelaysFirst(t *testing.T) {
	store := NewStoreWithRanking(RankingModeLegacy)
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	ipv4Req := validRegisterRequest()
	ipv4Req.PublicHost = "203.0.113.10"
	if _, err := store.Register(ipv4Req, now.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("register ipv4 relay: %v", err)
	}

	ipv6Req := validRegisterRequest()
	ipv6Req.PublicHost = "2001:db8::443"
	if _, err := store.Register(ipv6Req, now, time.Minute); err != nil {
		t.Fatalf("register ipv6 relay: %v", err)
	}

	got, err := store.List(now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 relays, got %d", len(got))
	}
	if got[0].PublicHost != "2001:db8::443" {
		t.Fatalf("expected legacy IPv6 relay first, got %q", got[0].PublicHost)
	}
}

func TestStoreGlobalRankingPrefersLowerActiveLoad(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	crowded := registerRelayForRanking(t, store, now, "crowded.example.com", 1, 20)
	open := registerRelayForRanking(t, store, now.Add(time.Second), "open.example.com", 8, 20)

	recordRelayMetric(t, store, now.Add(10*time.Second), TelemetryEvent{
		EventID:    "heartbeat-1",
		Event:      "session_heartbeat",
		OccurredAt: now.Add(10 * time.Second),
		ClientID:   "client-1",
		SessionID:  "session-1",
		RelayID:    crowded.ID,
	})

	got := listRelayIDs(t, store, now.Add(20*time.Second))
	if got[0] != open.ID {
		t.Fatalf("expected open relay first, got %v", got)
	}
}

func TestStoreGlobalRankingDemotesRecentFailures(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	reliable := registerRelayForRanking(t, store, now, "reliable.example.com", 8, 20)
	failing := registerRelayForRanking(t, store, now.Add(time.Second), "failing.example.com", 8, 20)

	recordRelayMetric(t, store, now.Add(10*time.Second), TelemetryEvent{
		EventID:    "success-1",
		Event:      "relay_failover",
		OccurredAt: now.Add(10 * time.Second),
		ClientID:   "client-1",
		SessionID:  "session-1",
		RelayID:    reliable.ID,
	})
	recordRelayMetric(t, store, now.Add(11*time.Second), TelemetryEvent{
		EventID:    "failure-1",
		Event:      "relay_attempt_failed",
		OccurredAt: now.Add(11 * time.Second),
		ClientID:   "client-2",
		SessionID:  "session-2",
		RelayID:    failing.ID,
	})

	got := listRelayIDs(t, store, now.Add(20*time.Second))
	if got[0] != reliable.ID {
		t.Fatalf("expected reliable relay first, got %v", got)
	}
}

func TestStoreGlobalRankingPrefersLowerLatency(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fast := registerRelayForRanking(t, store, now, "fast.example.com", 8, 20)
	slow := registerRelayForRanking(t, store, now.Add(time.Second), "slow.example.com", 8, 20)

	recordRelayMetric(t, store, now.Add(10*time.Second), TelemetryEvent{
		EventID:      "fast-success",
		Event:        "connection_succeeded",
		OccurredAt:   now.Add(10 * time.Second),
		ClientID:     "client-1",
		SessionID:    "session-1",
		RelayID:      fast.ID,
		Measurements: map[string]int64{"relay_tcp_ms": 60, "internet_probe_ms": 120},
	})
	recordRelayMetric(t, store, now.Add(11*time.Second), TelemetryEvent{
		EventID:      "slow-success",
		Event:        "connection_succeeded",
		OccurredAt:   now.Add(11 * time.Second),
		ClientID:     "client-2",
		SessionID:    "session-2",
		RelayID:      slow.ID,
		Measurements: map[string]int64{"relay_tcp_ms": 1500, "internet_probe_ms": 1800},
	})

	got := listRelayIDs(t, store, now.Add(20*time.Second))
	if got[0] != fast.ID {
		t.Fatalf("expected fast relay first, got %v", got)
	}
}

func TestStoreGlobalRankingUsesSpeedTests(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fast := registerRelayForRanking(t, store, now, "fast-download.example.com", 8, 20)
	slow := registerRelayForRanking(t, store, now.Add(time.Second), "slow-download.example.com", 8, 20)

	recordRelayMetric(t, store, now.Add(10*time.Second), TelemetryEvent{
		EventID:      "fast-speed",
		Event:        "speed_test_completed",
		OccurredAt:   now.Add(10 * time.Second),
		ClientID:     "client-1",
		SessionID:    "session-1",
		RelayID:      fast.ID,
		Measurements: map[string]int64{"download_mbps_milli": 20_000, "time_to_first_byte_ms": 120},
	})
	recordRelayMetric(t, store, now.Add(11*time.Second), TelemetryEvent{
		EventID:      "slow-speed",
		Event:        "speed_test_completed",
		OccurredAt:   now.Add(11 * time.Second),
		ClientID:     "client-2",
		SessionID:    "session-2",
		RelayID:      slow.ID,
		Measurements: map[string]int64{"download_mbps_milli": 2_000, "time_to_first_byte_ms": 120},
	})

	got := listRelayIDs(t, store, now.Add(20*time.Second))
	if got[0] != fast.ID {
		t.Fatalf("expected faster download relay first, got %v", got)
	}
}

func registerRelayForRanking(t *testing.T, store *Store, now time.Time, host string, maxSessions, maxMbps int) relay.Descriptor {
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

func recordRelayMetric(t *testing.T, store *Store, receivedAt time.Time, event TelemetryEvent) {
	t.Helper()
	if err := store.RecordRelayTelemetry(context.Background(), []TelemetryRecord{{
		ReceivedAt: receivedAt,
		SourceIP:   "198.51.100.10",
		Event:      event,
	}}, receivedAt); err != nil {
		t.Fatalf("record relay telemetry: %v", err)
	}
}

func listRelayIDs(t *testing.T, store *Store, now time.Time) []string {
	t.Helper()
	relays, err := store.List(now, 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	ids := make([]string, len(relays))
	for index, desc := range relays {
		ids[index] = desc.ID
	}
	return ids
}

func TestStoreRoundTripsPunchCapable(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 6, 9, 7, 0, 0, 0, time.UTC)

	req := validRegisterRequest()
	req.Transport = relay.TransportTunnel
	req.PunchCapable = true
	req.PunchEndpoint = "https://203.0.113.1:9444"

	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	if !desc.PunchCapable {
		t.Fatal("register did not carry punch_capable")
	}
	if desc.PunchEndpoint != "https://203.0.113.1:9444" {
		t.Fatalf("register did not carry punch_endpoint: %q", desc.PunchEndpoint)
	}

	listed, err := store.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(listed) != 1 || !listed[0].PunchCapable || listed[0].PunchEndpoint != "https://203.0.113.1:9444" {
		t.Fatalf("listed relay lost punch fields: %+v", listed)
	}

	// A relay that does not advertise it stays false (default).
	plain := validRegisterRequest()
	plain.PublicPort = 8443
	plainDesc, err := store.Register(plain, now, time.Minute)
	if err != nil {
		t.Fatalf("register plain relay: %v", err)
	}
	if plainDesc.PunchCapable {
		t.Fatal("plain relay unexpectedly punch_capable")
	}
}

func validRegisterRequest() relay.RegisterRequest {
	return relay.RegisterRequest{
		PublicHost:       "volunteer.example.com",
		PublicPort:       443,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
		RealityPublicKey: "public-key",
		ShortID:          "5f7a8d9c01ab23cd",
		ServerName:       "www.cloudflare.com",
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		MaxSessions:      8,
		MaxMbps:          20,
		VolunteerVersion: "test",
	}
}
