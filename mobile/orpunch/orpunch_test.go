package orpunch_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/punch"
	"openrung/mobile/orpunch"
)

// fakeHub stands up a punch coordinator faithful to internal/tunnel/punch.go but
// with the volunteer running in-process: /request mints the session token, drives
// the real punch.RespondToDirective (which punches back and serves QUIC bridged
// to a loopback echo), and returns its ack. It lets orpunch.Dial exercise the
// entire real client punch path against the real server code over loopback.
type fakeHub struct {
	secret    []byte
	reflector *punch.Reflector
	echoHost  string
	echoPort  int
	ctx       context.Context
	relayID   string
}

func (h *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(punch.PathPunchConfig, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, punch.PunchConfig{
			ReflectorAddrs: h.reflector.Addrs(),
			ALPN:           punch.ALPN,
			TTLMillis:      2000,
		})
	})
	mux.HandleFunc(punch.PathPunchRequest, func(w http.ResponseWriter, r *http.Request) {
		var req punch.PunchRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			writeJSON(w, punch.PunchResponse{OK: false, Error: "invalid request"})
			return
		}

		// Classify the client from the hub's own reflector observations, exactly
		// as the real coordinator does — the client's Gather must have populated
		// them by now.
		var clientReflexive []punch.Endpoint
		clientClass := punch.ClassUnknown
		if key, err := punch.NonceKey(req.ClientNonce); err == nil {
			if class, reflexive, ok := h.reflector.Classify(key); ok {
				clientClass = class
				clientReflexive = punch.SanitizePeers(reflexive)
			}
		}

		sessionID := "sess-" + strconv.FormatInt(int64(len(req.ClientNonce)), 10)
		token := punch.EncodeToken(punch.ComputeToken(h.secret, sessionID, req.RelayID, req.ClientNonce))

		dir := punch.PunchDirective{
			SessionID:       sessionID,
			RelayID:         req.RelayID,
			ClientReflexive: clientReflexive,
			ClientLocal:     punch.SanitizePeers(req.ClientLocal),
			ClientClass:     clientClass,
			PunchToken:      token,
			ReflectorAddrs:  h.reflector.Addrs(),
			TTLMillis:       2000,
			QUICALPN:        punch.ALPN,
			ProtoVersion:    punch.ProtoVersion,
		}

		// The volunteer's punch/bridge goroutine must outlive this request, so use
		// the test context, not r.Context().
		ack := punch.RespondToDirective(h.ctx, dir, h.echoHost, h.echoPort, discard())
		if !ack.OK {
			writeJSON(w, punch.PunchResponse{OK: false, Error: "volunteer declined: " + ack.Error})
			return
		}
		writeJSON(w, punch.PunchResponse{
			OK:                 true,
			SessionID:          sessionID,
			VolunteerReflexive: ack.VolunteerReflexive,
			VolunteerLocal:     ack.VolunteerLocal,
			VolunteerClass:     ack.VolunteerClass,
			PunchToken:         token,
			CertFingerprint:    ack.CertFingerprint,
			TTLMillis:          2000,
		})
	})
	return mux
}

func TestDialEndToEndOverLoopback(t *testing.T) {
	echoHost, echoPort := startEchoServer(t)

	reflector, err := punch.NewReflector([]string{"127.0.0.1:0"}, discard())
	if err != nil {
		t.Fatalf("reflector: %v", err)
	}
	defer reflector.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := httptest.NewServer((&fakeHub{
		secret:    []byte("test-hub-session-secret-32-bytes!"),
		reflector: reflector,
		echoHost:  echoHost,
		echoPort:  echoPort,
		ctx:       ctx,
		relayID:   "relay_loopback",
	}).handler())
	defer hub.Close()

	var protectCalls int32
	protector := protectorFunc(func(fd int) {
		if fd <= 0 {
			t.Errorf("protector got non-positive fd %d", fd)
		}
		atomic.AddInt32(&protectCalls, 1)
	})

	session, err := orpunch.Dial(&orpunch.Config{HubBaseURL: hub.URL, RelayID: "relay_loopback"}, protector)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer session.Close()

	if !session.OK() {
		t.Fatalf("punch did not succeed: reason=%q nat=%q", session.Reason(), session.NATClass())
	}
	if got := atomic.LoadInt32(&protectCalls); got != 1 {
		t.Fatalf("Protect called %d times, want 1", got)
	}
	if session.BridgeHost() != "127.0.0.1" || session.BridgePort() <= 0 {
		t.Fatalf("bad bridge endpoint %s:%d", session.BridgeHost(), session.BridgePort())
	}
	if net.ParseIP(session.PeerIP()) == nil {
		t.Fatalf("PeerIP %q is not an IP", session.PeerIP())
	}

	// Bytes must flow client -> loopback bridge -> QUIC -> volunteer bridge -> echo.
	if err := echoRoundTrip(session.BridgeHost(), session.BridgePort(), "hello-through-punch"); err != nil {
		t.Fatalf("echo over punched path: %v", err)
	}
	if err := echoRoundTrip(session.BridgeHost(), session.BridgePort(), "second-stream"); err != nil {
		t.Fatalf("second stream over punched path: %v", err)
	}
}

func TestDialFallsBackWhenHubDeclines(t *testing.T) {
	// A hub whose /request always declines: Dial must return a non-OK Session with
	// a structured reason, NOT an error, so the caller falls back to the relay.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == punch.PathPunchConfig {
			writeJSON(w, punch.PunchConfig{ReflectorAddrs: []string{"127.0.0.1:1"}, ALPN: punch.ALPN, TTLMillis: 500})
			return
		}
		writeJSON(w, punch.PunchResponse{OK: false, Error: "no volunteer"})
	}))
	defer hub.Close()

	session, err := orpunch.Dial(&orpunch.Config{HubBaseURL: hub.URL, RelayID: "relay_x"}, protectorFunc(func(int) {}))
	if err != nil {
		t.Fatalf("Dial returned error (should be a non-OK Session): %v", err)
	}
	defer session.Close()
	if session.OK() {
		t.Fatal("expected punch to fail against a declining hub")
	}
	if session.Reason() == "" {
		t.Fatal("expected a non-empty failure reason for telemetry")
	}
}

func TestDialRejectsBadConfig(t *testing.T) {
	if _, err := orpunch.Dial(nil, protectorFunc(func(int) {})); err == nil {
		t.Fatal("expected error for nil config")
	}
	if _, err := orpunch.Dial(&orpunch.Config{RelayID: "r"}, protectorFunc(func(int) {})); err == nil {
		t.Fatal("expected error for empty HubBaseURL")
	}
	if _, err := orpunch.Dial(&orpunch.Config{HubBaseURL: "http://x", RelayID: "r"}, nil); err == nil {
		t.Fatal("expected error for nil protector")
	}
}

// --- helpers ---

type protectorFunc func(fd int)

func (f protectorFunc) Protect(fd int) { f(fd) }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func startEchoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func echoRoundTrip(host string, port int, msg string) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != msg {
		return io.ErrUnexpectedEOF
	}
	return nil
}
