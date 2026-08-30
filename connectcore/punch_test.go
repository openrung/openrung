package connectcore

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"
)

func TestPunchBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		override string
		relay    brokerapi.RelayDescriptor
		want     string
	}{
		{
			name:  "advertised punch_endpoint wins over derivation",
			relay: brokerapi.RelayDescriptor{PublicHost: "43.201.124.63", PunchEndpoint: "https://43.201.124.63:9444"},
			want:  "https://43.201.124.63:9444",
		},
		{
			name:     "explicit override beats everything",
			override: "https://hub.example:8443",
			relay:    brokerapi.RelayDescriptor{PublicHost: "43.201.124.63", PunchEndpoint: "https://43.201.124.63:9444"},
			want:     "https://hub.example:8443",
		},
		{
			name:  "legacy fallback when no endpoint advertised",
			relay: brokerapi.RelayDescriptor{PublicHost: "203.0.113.5"},
			want:  "http://203.0.113.5:9444",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := punchBaseURL(c.override, c.relay); got != c.want {
				t.Fatalf("punchBaseURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPunchHTTPClientInsecure(t *testing.T) {
	if punchHTTPClient(false, nil) != nil {
		t.Fatal("secure mode should return nil (default client)")
	}
	c := punchHTTPClient(true, nil)
	if c == nil {
		t.Fatal("insecure mode should return a client")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("insecure client should skip TLS verification: %+v", c.Transport)
	}
}

func TestNewVerifiesPunchHubTLS(t *testing.T) {
	// The engine default is secure; a host that talks to a self-signed hub
	// (the desktop app) opts out explicitly.
	if New().PunchInsecure {
		t.Fatal("PunchInsecure should default to false")
	}
}

func TestAttemptPunchSkipsWhenDisabledOrIncapable(t *testing.T) {
	// Disabled globally.
	if est := AttemptPunch(t.Context(), nil, brokerapi.RelayDescriptor{PunchCapable: true}, PunchOptions{Enabled: false}); est != nil {
		t.Fatal("punch should be skipped when disabled")
	}

	// Enabled but relay is not punch-capable (a direct relay) — no hub call.
	if est := AttemptPunch(t.Context(), nil, brokerapi.RelayDescriptor{PunchCapable: false}, PunchOptions{Enabled: true}); est != nil {
		t.Fatal("punch should be skipped for a non-punch-capable relay")
	}
}

func TestPunchPathCloseNilSafe(t *testing.T) {
	// Teardown must never panic on a defensively-handled establishment.
	if err := (*PunchPath)(nil).Close(); err != nil {
		t.Fatalf("nil path Close() = %v", err)
	}
	if err := (&PunchPath{}).Close(); err != nil {
		t.Fatalf("bridgeless path Close() = %v", err)
	}
}

func TestAttemptPunchUnwiredEstablisherIsObservable(t *testing.T) {
	// Enabled + punch-capable + no establisher is a host misconfiguration:
	// the hub fallback must be taken, and the skip must leave a log trail
	// (and a punch_skipped event, exercised here through the nil-safe mgr).
	var logs []string
	est := AttemptPunch(t.Context(), nil, brokerapi.RelayDescriptor{ID: "relay-a", PunchCapable: true}, PunchOptions{
		Enabled: true,
		Log:     func(line string) { logs = append(logs, line) },
	})
	if est != nil {
		t.Fatalf("punch without an establisher returned a path: %+v", est)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "no establisher") {
		t.Fatalf("unwired establisher logs = %q, want one no-establisher line", logs)
	}
}

func TestAttemptPunchRejectsInvalidEstablishment(t *testing.T) {
	// An establisher that "succeeds" without a usable bridge is degraded to
	// the hub fallback instead of panicking on first Bridge use.
	var notices []Notice
	for name, path := range map[string]*PunchPath{
		"nil path":   nil,
		"nil bridge": {BridgeHost: "127.0.0.1", BridgePort: 1},
	} {
		est := AttemptPunch(t.Context(), nil, brokerapi.RelayDescriptor{ID: "relay-a", PunchCapable: true}, PunchOptions{
			Enabled: true,
			Notify:  func(n Notice) { notices = append(notices, n) },
			Establish: func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
				return path, punchcore.PunchResult{NATClass: "eim"}, nil
			},
		})
		if est != nil {
			t.Fatalf("%s: invalid establishment was accepted: %+v", name, est)
		}
	}
	if len(notices) != 2 {
		t.Fatalf("punch notices = %+v, want one failure notice per case", notices)
	}
	for _, n := range notices {
		if n.Kind != NoticePunchOutcome || !strings.Contains(n.Reason, "invalid establishment") {
			t.Fatalf("punch notice = %+v, want an invalid-establishment failure", n)
		}
	}
}

func TestMaybePunchPassesEstablisherThrough(t *testing.T) {
	// The engine must hand its configured PunchEstablisher to AttemptPunch;
	// dropping the pass-through would silently disable punch in every host.
	s := New()
	s.PunchEnabled = true
	called := false
	s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
		called = true
		return nil, punchcore.PunchResult{Reason: "config"}, context.Canceled
	}
	if est := s.maybePunch(t.Context(), &connection{}, brokerapi.RelayDescriptor{ID: "relay-a", PunchCapable: true}); est != nil {
		t.Fatalf("failing establisher returned a path: %+v", est)
	}
	if !called {
		t.Fatal("maybePunch never invoked the engine's PunchEstablisher")
	}
}

func TestMaybePunchSkipsWhenDisabledOrIncapable(t *testing.T) {
	// Disabled globally.
	s := New()
	s.PunchEnabled = false
	if est := s.maybePunch(t.Context(), &connection{}, brokerapi.RelayDescriptor{PunchCapable: true}); est != nil {
		t.Fatal("punch should be skipped when disabled")
	}

	// Enabled but relay is not punch-capable (a direct relay) — no hub call.
	s2 := New()
	if est := s2.maybePunch(t.Context(), &connection{}, brokerapi.RelayDescriptor{PunchCapable: false}); est != nil {
		t.Fatal("punch should be skipped for a non-punch-capable relay")
	}
}
