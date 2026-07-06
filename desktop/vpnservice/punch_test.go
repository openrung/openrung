package vpnservice

import (
	"net/http"
	"testing"

	"openrung/internal/relay"
)

func TestPunchBaseURL(t *testing.T) {
	cases := []struct {
		name  string
		relay relay.Descriptor
		want  string
	}{
		{
			name:  "advertised punch_endpoint wins",
			relay: relay.Descriptor{PublicHost: "43.201.124.63", PunchEndpoint: "https://43.201.124.63:9444"},
			want:  "https://43.201.124.63:9444",
		},
		{
			name:  "legacy fallback when no endpoint advertised",
			relay: relay.Descriptor{PublicHost: "203.0.113.5"},
			want:  "http://203.0.113.5:9444",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := punchBaseURL(c.relay); got != c.want {
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

func TestMaybePunchSkipsWhenDisabledOrIncapable(t *testing.T) {
	// Disabled globally.
	s := New()
	s.PunchEnabled = false
	if est := s.maybePunch(t.Context(), nil, relay.Descriptor{PunchCapable: true}); est != nil {
		t.Fatal("punch should be skipped when disabled")
	}

	// Enabled but relay is not punch-capable (a direct relay) — no hub call.
	s2 := New()
	if est := s2.maybePunch(t.Context(), nil, relay.Descriptor{PunchCapable: false}); est != nil {
		t.Fatal("punch should be skipped for a non-punch-capable relay")
	}
}
