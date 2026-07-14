package client

import (
	"errors"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestSelectRelaySelectsFirstUsableRelay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	wrongProtocol := validRelay(now)
	wrongProtocol.ID = "wrong_protocol"
	wrongProtocol.Protocol = "unknown"

	second := validRelay(now)
	second.ID = "selected"

	selected, err := SelectRelay(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{wrongProtocol, second},
	})
	if err != nil {
		t.Fatalf("select relay: %v", err)
	}
	if selected.ID != "selected" {
		t.Fatalf("expected selected relay, got %q", selected.ID)
	}
}

func TestSelectRelaySkipsExpiredRelay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	expired := validRelay(now)
	expired.ExpiresAt = now.Add(-time.Second)

	_, err := SelectRelay(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{expired},
	})
	if !errors.Is(err, ErrNoUsableRelay) {
		t.Fatalf("expected ErrNoUsableRelay, got %v", err)
	}
}

func TestSelectRelaySkipsIncompleteRelay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	incomplete := validRelay(now)
	incomplete.RealityPublicKey = ""

	_, err := SelectRelay(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{incomplete},
	})
	if !errors.Is(err, ErrNoUsableRelay) {
		t.Fatalf("expected ErrNoUsableRelay, got %v", err)
	}
}

func TestIsUsableRelayRequiresDirectVLESSRealityVision(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	candidate := validRelay(now)
	candidate.ExitMode = relay.ExitModeDedicated

	if IsUsableRelay(candidate, now) {
		t.Fatal("expected dedicated exit relay to be unusable for MVP client")
	}
}

func TestSelectRelayAutoPreservesBrokerOrder(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ipv4 := validRelay(now)
	ipv4.ID = "ipv4"
	ipv4.PublicHost = "203.0.113.10"

	ipv6 := validRelay(now)
	ipv6.ID = "ipv6"
	ipv6.PublicHost = "2001:db8::443"

	selected, err := SelectRelay(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{ipv4, ipv6},
	})
	if err != nil {
		t.Fatalf("select relay: %v", err)
	}
	if selected.ID != "ipv4" {
		t.Fatalf("expected first broker-ranked relay, got %q", selected.ID)
	}
}

func TestSelectRelayForFamilyCanPreferIPv4Relay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ipv4 := validRelay(now)
	ipv4.ID = "ipv4"
	ipv4.PublicHost = "203.0.113.10"

	ipv6 := validRelay(now)
	ipv6.ID = "ipv6"
	ipv6.PublicHost = "2001:db8::443"

	selected, err := SelectRelayForFamily(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{ipv6, ipv4},
	}, RelayFamilyIPv4)
	if err != nil {
		t.Fatalf("select relay: %v", err)
	}
	if selected.ID != "ipv4" {
		t.Fatalf("expected ipv4 relay, got %q", selected.ID)
	}
}

func TestSelectRelayForFamilyCanPreferIPv6Relay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ipv4 := validRelay(now)
	ipv4.ID = "ipv4"
	ipv4.PublicHost = "203.0.113.10"

	ipv6 := validRelay(now)
	ipv6.ID = "ipv6"
	ipv6.PublicHost = "2001:db8::443"

	selected, err := SelectRelayForFamily(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{ipv4, ipv6},
	}, RelayFamilyIPv6)
	if err != nil {
		t.Fatalf("select relay: %v", err)
	}
	if selected.ID != "ipv6" {
		t.Fatalf("expected ipv6 relay, got %q", selected.ID)
	}
}

func TestSelectRelayForFamilyRequiresMatchingFamily(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ipv6 := validRelay(now)
	ipv6.PublicHost = "2001:db8::443"

	_, err := SelectRelayForFamily(relay.ListResponse{
		ServerTime: now,
		Relays:     []relay.Descriptor{ipv6},
	}, RelayFamilyIPv4)
	if !errors.Is(err, ErrNoUsableRelay) {
		t.Fatalf("expected ErrNoUsableRelay, got %v", err)
	}
}

func TestParseRelayFamilyRejectsUnknownValue(t *testing.T) {
	_, err := ParseRelayFamily("fast")
	if err == nil {
		t.Fatal("expected unknown relay family to fail")
	}
}

func validRelay(now time.Time) relay.Descriptor {
	return relay.Descriptor{
		ID:               "relay_123",
		PublicHost:       "relay.example.com",
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
		RelayVersion:     "dev",
		RegisteredAt:     now.Add(-time.Minute),
		LastHeartbeatAt:  now.Add(-time.Second),
		ExpiresAt:        now.Add(time.Minute),
	}
}
