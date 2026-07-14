package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openrung/internal/tunnel"
)

// stubIPv6 temporarily replaces public-IPv6 detection for a test.
func stubIPv6(t *testing.T, addr string, err error) {
	t.Helper()
	prev := detectPublicIPv6
	detectPublicIPv6 = func() (string, error) { return addr, err }
	t.Cleanup(func() { detectPublicIPv6 = prev })
}

func setReprobeInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := autoReprobeInterval
	autoReprobeInterval = d
	t.Cleanup(func() { autoReprobeInterval = prev })
}

// reachableHub returns a Config whose probe resolves as directly reachable, via
// a live reachability prober on loopback.
func reachableHub(t *testing.T) Config {
	t.Helper()
	prober := tunnel.NewReachabilityProber("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return Config{HubHTTPURL: ts.URL, HTTPClient: ts.Client(), ListenPort: freePort(t)}
}

// TestWatchPromotesTunnelToDirectWhenReachable proves the recovery path: a
// tunnelling session re-probes and, once the hub confirms this machine is
// reachable, signals a switch to direct — so a machine is never stuck in tunnel
// after it becomes reachable (the original P1), without any speculation.
func TestWatchPromotesTunnelToDirectWhenReachable(t *testing.T) {
	setReprobeInterval(t, 20*time.Millisecond)
	eng := New(Config{}, Events{})
	ch := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.watchForModeChange(ctx, reachableHub(t), ModeTunnel, ch)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never signalled tunnel→direct despite the hub confirming reachability")
	}
}

// TestWatchNoSwitchWhenModeUnchanged proves the watcher does not thrash: while
// already direct and still reachable, it never signals a spurious switch.
func TestWatchNoSwitchWhenModeUnchanged(t *testing.T) {
	setReprobeInterval(t, 20*time.Millisecond)
	eng := New(Config{}, Events{})
	ch := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.watchForModeChange(ctx, reachableHub(t), ModeDirect, ch)
	select {
	case <-ch:
		t.Fatal("watcher signalled a switch even though the resolved mode was unchanged")
	case <-time.After(500 * time.Millisecond):
	}
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

// TestAutoResolveHubDownNeverGuessesDirect is the regression for the dead-relay
// hazard: when the hub is unreachable the probe cannot verify reachability, so
// auto mode must NOT speculatively advertise a direct address — even when the
// machine has a public IPv6 (which does not imply inbound reachability). It
// tunnels and retries instead, and the periodic re-probe promotes to direct only
// once the hub confirms reachability. Guessing direct here would strand a
// firewalled machine as a permanently-advertised dead relay.
func TestAutoResolveHubDownNeverGuessesDirect(t *testing.T) {
	// Even with a public IPv6 present, a hub-down probe error must yield tunnel,
	// never a speculative direct.
	stubIPv6(t, "2001:db8::1", nil)
	eng := New(Config{}, Events{})
	mode, host := eng.autoResolve(context.Background(), Config{
		HubHTTPURL: "http://127.0.0.1:1", // refuses connections
		HTTPClient: &http.Client{},
		ListenPort: freePort(t),
	})
	if mode != ModeTunnel {
		t.Fatalf("hub down + IPv6 → mode = %q, want tunnel (no speculative direct)", mode)
	}
	if host != "" {
		t.Fatalf("hub down → host = %q, want empty (nothing verified to advertise)", host)
	}
}
