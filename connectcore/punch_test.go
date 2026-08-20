package connectcore

import (
	"net/http"
	"testing"

	"github.com/openrung/openrung/brokerapi"
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
	if punchHTTPClient(false) != nil {
		t.Fatal("secure mode should return nil (default client)")
	}
	c := punchHTTPClient(true)
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

func TestMaybePunchSkipsWhenDisabledOrIncapable(t *testing.T) {
	// Disabled globally.
	s := New()
	s.PunchEnabled = false
	if est := s.maybePunch(t.Context(), nil, brokerapi.RelayDescriptor{PunchCapable: true}); est != nil {
		t.Fatal("punch should be skipped when disabled")
	}

	// Enabled but relay is not punch-capable (a direct relay) — no hub call.
	s2 := New()
	if est := s2.maybePunch(t.Context(), nil, brokerapi.RelayDescriptor{PunchCapable: false}); est != nil {
		t.Fatal("punch should be skipped for a non-punch-capable relay")
	}
}
