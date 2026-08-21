package relayruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"

	"openrung/internal/tunnel"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestProbeDirectReachabilityReachable wires the real hub prober to the relay
// detection client on loopback: the relay opens its temporary listener, the hub
// dials it back at the observed source IP, and the probe reports reachable.
func TestProbeDirectReachabilityReachable(t *testing.T) {
	prober := tunnel.NewReachabilityProber("token123", testLogger())
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := freeTCPPort(t)
	result := ProbeDirectReachability(context.Background(), ts.URL, "token123", "::", port, ts.Client())
	if result.Err != nil {
		t.Fatalf("probe: %v", result.Err)
	}
	if result.Outcome != DirectProbeReachable {
		t.Fatalf("outcome = %q, want reachable on loopback", result.Outcome)
	}
	if result.ObservedHost != "127.0.0.1" {
		t.Fatalf("observed host = %q, want 127.0.0.1", result.ObservedHost)
	}
}

// TestProbeDirectReachabilityHubDown returns an inconclusive error (not a false
// "reachable") when the hub HTTP API cannot be reached.
func TestProbeDirectReachabilityHubDown(t *testing.T) {
	// A URL that refuses connections.
	deadURL := "http://127.0.0.1:1"
	port := freeTCPPort(t)
	result := ProbeDirectReachability(context.Background(), deadURL, "", "::", port, &http.Client{})
	if result.Err == nil {
		t.Fatal("expected an error when the hub is unreachable")
	}
	if result.Outcome == DirectProbeReachable {
		t.Fatal("must not report reachable when the probe could not run")
	}
}

func TestProbeDirectReachabilityClassifiesOccupiedPort(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	result := ProbeDirectReachability(context.Background(), "http://127.0.0.1:1", "", "::", port, &http.Client{})

	if result.Outcome != DirectProbePortInUse {
		t.Fatalf("occupied port outcome = %q (%v), want %q", result.Outcome, result.Err, DirectProbePortInUse)
	}
	if result.Err == nil {
		t.Fatal("occupied port result has nil error")
	}
}

func TestProbeDirectReachabilityClassifiesExternalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"reachable":false,"observed_host":"198.51.100.1"}`))
	}))
	defer server.Close()

	result := ProbeDirectReachability(context.Background(), server.URL, "", "::", freeTCPPort(t), server.Client())

	if result.Outcome != DirectProbeExternallyUnreachable {
		t.Fatalf("negative callback outcome = %q (%v), want %q", result.Outcome, result.Err, DirectProbeExternallyUnreachable)
	}
	if result.Err != nil {
		t.Fatalf("negative callback error = %v, want nil", result.Err)
	}
	if result.ObservedHost != "198.51.100.1" {
		t.Fatalf("observed host = %q, want 198.51.100.1", result.ObservedHost)
	}
}

func TestClassifyProbeBindErrorPermissionDenied(t *testing.T) {
	err := &os.PathError{Op: "listen", Path: ":443", Err: os.ErrPermission}
	if got := classifyProbeBindError(err); got != DirectProbePermissionDenied {
		t.Fatalf("permission outcome = %q, want %q", got, DirectProbePermissionDenied)
	}
	if got := classifyProbeBindError(errors.New("unrelated bind failure")); got != DirectProbeBindFailed {
		t.Fatalf("generic bind outcome = %q, want %q", got, DirectProbeBindFailed)
	}
	if got := classifyProbeBindError(fmt.Errorf("wrapped: %w", syscall.EADDRINUSE)); got != DirectProbePortInUse {
		t.Fatalf("EADDRINUSE outcome = %q, want %q", got, DirectProbePortInUse)
	}
}

func TestDeriveHubHTTPBase(t *testing.T) {
	cases := []struct {
		explicit, hub string
		useTLS        bool
		want          string
	}{
		{"", "hub.example:9443", false, "http://hub.example:9444"},
		{"", "hub.example:9443", true, "https://hub.example:9444"},
		{"https://hub.example:8443", "hub.example:9443", false, "https://hub.example:8443"},
		{"", "hub.example", true, "https://hub.example:9444"},
		{"", "203.0.113.5:9443", false, "http://203.0.113.5:9444"},
	}
	for _, c := range cases {
		if got := DeriveHubHTTPBase(c.explicit, c.hub, c.useTLS); got != c.want {
			t.Errorf("DeriveHubHTTPBase(%q, %q, %v) = %q, want %q", c.explicit, c.hub, c.useTLS, got, c.want)
		}
	}
}

func TestProbeBindAddr(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"", 443, ":443"},
		{"::", 443, ":443"},
		{"dual", 443, ":443"},
		{"both", 443, ":443"},
		{"10.0.0.5", 443, "10.0.0.5:443"},
		{"0.0.0.0", 443, "0.0.0.0:443"},
	}
	for _, c := range cases {
		if got := ProbeBindAddr(c.host, c.port); got != c.want {
			t.Errorf("ProbeBindAddr(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}
