package punch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDialerControlProtectsSocket verifies that a non-nil Dialer.Control is
// invoked exactly once, with a live file descriptor, when the punch socket is
// created — the seam Android uses to VpnService.protect() the socket. The punch
// itself is expected to fail (the fake hub only answers /config and 404s the
// punch request), which is fine: the socket is opened, and thus Control is fired,
// before the request that fails.
func TestDialerControlProtectsSocket(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathPunchConfig:
			// One unreachable reflector so Gather (on the just-protected socket)
			// returns quickly; Establish then fails at the unhandled punch request.
			writeJSON(t, w, PunchConfig{ReflectorAddrs: []string{"127.0.0.1:1"}, ALPN: ALPN, TTLMillis: 500})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer hub.Close()

	var gotFD uintptr
	calls := 0
	dialer := &Dialer{
		Hub:     HubClient{BaseURL: hub.URL},
		RelayID: "relay_test",
		Logger:  discardLogger(),
		Control: func(fd uintptr) {
			calls++
			gotFD = fd
		},
	}

	_, _, err := dialer.Establish(context.Background())
	if err == nil {
		t.Fatal("expected Establish to fail with an unreachable reflector")
	}
	if calls != 1 {
		t.Fatalf("Control called %d times, want exactly 1", calls)
	}
	if gotFD == 0 {
		t.Fatalf("Control got fd 0, want a live descriptor")
	}
}

// TestDialerNilControlOpensUDPConn confirms the desktop path (nil Control) still
// yields a working *net.UDPConn from listenPunchSocket.
func TestDialerNilControlOpensUDPConn(t *testing.T) {
	dialer := &Dialer{Logger: discardLogger()}
	sock, err := dialer.listenPunchSocket(context.Background())
	if err != nil {
		t.Fatalf("listenPunchSocket: %v", err)
	}
	defer sock.Close()
	if sock.LocalAddr() == nil {
		t.Fatal("socket has no local address")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode json: %v", err)
	}
}
