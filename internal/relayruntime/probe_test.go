package relayruntime

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestDetectDirectReachableReachable wires the real hub prober to the relay
// detection client on loopback: the relay opens its temporary listener, the hub
// dials it back at the observed source IP, and detection reports reachable.
func TestDetectDirectReachableReachable(t *testing.T) {
	prober := tunnel.NewReachabilityProber("token123", testLogger())
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := freeTCPPort(t)
	reachable, host, err := DetectDirectReachable(context.Background(), ts.URL, "token123", "::", port, ts.Client())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !reachable {
		t.Fatal("expected reachable on loopback")
	}
	if host != "127.0.0.1" {
		t.Fatalf("observed host = %q, want 127.0.0.1", host)
	}
}

// TestDetectDirectReachableHubDown returns an inconclusive error (not a false
// "reachable") when the hub HTTP API cannot be reached.
func TestDetectDirectReachableHubDown(t *testing.T) {
	// A URL that refuses connections.
	deadURL := "http://127.0.0.1:1"
	port := freeTCPPort(t)
	reachable, _, err := DetectDirectReachable(context.Background(), deadURL, "", "::", port, &http.Client{})
	if err == nil {
		t.Fatal("expected an error when the hub is unreachable")
	}
	if reachable {
		t.Fatal("must not report reachable when the probe could not run")
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
