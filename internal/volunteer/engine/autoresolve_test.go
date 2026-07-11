package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"openrung/internal/tunnel"
)

// stubIPv6 temporarily replaces public-IPv6 detection for a test.
func stubIPv6(t *testing.T, addr string, err error) {
	t.Helper()
	prev := detectPublicIPv6
	detectPublicIPv6 = func() (string, error) { return addr, err }
	t.Cleanup(func() { detectPublicIPv6 = prev })
}

func TestAutoResolveReachableSelectsDirect(t *testing.T) {
	prober := tunnel.NewReachabilityProber("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	eng := New(Config{}, Events{})
	// Reachable machines must serve directly, advertising the hub-observed host.
	mode, host := eng.autoResolve(context.Background(), Config{
		HubHTTPURL: ts.URL,
		HTTPClient: ts.Client(),
		ListenPort: freePort(t),
	})
	if mode != ModeDirect {
		t.Fatalf("reachable → mode = %q, want direct", mode)
	}
	if host != "127.0.0.1" {
		t.Fatalf("reachable → host = %q, want 127.0.0.1", host)
	}
}

func TestAutoResolveHubDownWithIPv6SelectsDirect(t *testing.T) {
	// A hub that is unreachable (dead URL) means the probe errors. Since tunnel
	// mode also needs the hub, a machine with a public IPv6 must serve directly
	// rather than tunnel into a dead hub — this is the outage-recovery case.
	stubIPv6(t, "2001:db8::1", nil)
	eng := New(Config{}, Events{})
	mode, host := eng.autoResolve(context.Background(), Config{
		HubHTTPURL: "http://127.0.0.1:1", // refuses connections
		HTTPClient: &http.Client{},
		ListenPort: freePort(t),
	})
	if mode != ModeDirect {
		t.Fatalf("hub down + IPv6 → mode = %q, want direct", mode)
	}
	if host != "2001:db8::1" {
		t.Fatalf("hub down + IPv6 → host = %q, want the detected IPv6", host)
	}
}

func TestAutoResolveHubDownNoIPv6SelectsTunnel(t *testing.T) {
	// No public address to advertise: tunnel is the only option (keep retrying
	// the hub until it returns).
	stubIPv6(t, "", context.DeadlineExceeded)
	eng := New(Config{}, Events{})
	mode, host := eng.autoResolve(context.Background(), Config{
		HubHTTPURL: "http://127.0.0.1:1",
		HTTPClient: &http.Client{},
		ListenPort: freePort(t),
	})
	if mode != ModeTunnel {
		t.Fatalf("hub down + no IPv6 → mode = %q, want tunnel", mode)
	}
	if host != "" {
		t.Fatalf("hub down + no IPv6 → host = %q, want empty", host)
	}
}
