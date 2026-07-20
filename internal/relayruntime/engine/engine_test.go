package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
	"openrung/internal/tunnel"
)

var testIdentity = Identity{
	ClientID:          "b831381d-6324-4d53-ad4f-8cda48b30811",
	RealityPrivateKey: "yBaw3qcPC-EBiVzTF-3EbLpDF-eZ0lZ2pkc6y7NkoE4",
	RealityPublicKey:  "1S86-yqTP6d76nEWXXWlNAci9c9uFhYaSOFAIUYIljI",
	ShortID:           "0123456789abcdef",
	// Tests that call run*Session directly bypass prepareIdentity, which is
	// what fills this in for real callers.
	IdentitySeed: "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=",
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// fakeBroker implements the relay endpoints with configurable heartbeat failures.
type fakeBroker struct {
	mu           sync.Mutex
	registers    int
	heartbeats   int
	nextRelayID  int
	lastRegister relay.RegisterRequest
	// notFoundOnce makes the next heartbeat return the broker's pruned-relay
	// 404, then clears.
	notFoundOnce bool
}

func (f *fakeBroker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/relays/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.registers++
		f.nextRelayID++
		id := fmt.Sprintf("relay_%d", f.nextRelayID)
		var req relay.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastRegister = req
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(relay.Descriptor{ID: id, Label: req.Label, PublicHost: req.PublicHost, PublicPort: req.PublicPort})
	})
	mux.HandleFunc("/api/v1/relays/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.heartbeats++
		notFound := f.notFoundOnce
		f.notFoundOnce = false
		f.mu.Unlock()
		if notFound {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"relay not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func (f *fakeBroker) stats() (registers, heartbeats int, last relay.RegisterRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers, f.heartbeats, f.lastRegister
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestDirectSessionRegistersAndRecovers(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	var statuses []Status
	var mu sync.Mutex
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		Label:       "test-relay",
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{
		OnStatus: func(s Status) {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		},
	})
	// Direct mode auto-detects a public IPv6 which CI hosts may lack; pin the
	// session's public host through the auto-mode "observed" path instead by
	// injecting a fast heartbeat and running the exported surface only.
	eng.cfg.HeartbeatInterval = 50 * time.Millisecond
	brokerClient := &relayruntime.BrokerClient{BaseURL: ts.URL}
	// Bypass IPv6 detection: pretend probing already resolved us.
	go func() {
		_ = eng.runDirectSession(context.Background(), brokerClient, eng.cfg, "test-relay", testIdentity, "127.0.0.1")
	}()

	eventually(t, 5*time.Second, "online status", func() bool {
		return eng.Status().Phase == PhaseOnline && eng.Status().RelayID == "relay_1"
	})
	if got := eng.Status().Transport; got != relay.TransportDirect {
		t.Fatalf("transport = %q, want direct", got)
	}

	// Heartbeats flow.
	eventually(t, 5*time.Second, "heartbeats", func() bool {
		_, hb, _ := broker.stats()
		return hb >= 2
	})

	// A pruned lease (404) triggers transparent re-registration with a new ID.
	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool {
		return eng.Status().RelayID == "relay_2"
	})

	regs, _, last := broker.stats()
	if regs != 2 {
		t.Fatalf("registers = %d, want 2", regs)
	}
	if last.PublicHost != "127.0.0.1" {
		t.Fatalf("registered public host = %q, want 127.0.0.1", last.PublicHost)
	}
	if last.Label != "test-relay" {
		t.Fatalf("registered label = %q", last.Label)
	}
	mu.Lock()
	sawRegistering := false
	for _, s := range statuses {
		if s.Phase == PhaseRegistering {
			sawRegistering = true
		}
	}
	mu.Unlock()
	if !sawRegistering {
		t.Fatal("never observed registering phase")
	}
}

func TestEngineLifecycleStartStop(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeTunnel, // rejected: no hub → validation error path
		Identity:    testIdentity,
		DisableXray: true,
	}, Events{})
	if err := eng.Start(); err == nil {
		t.Fatal("expected validation error for tunnel mode without hub")
	}

	if eng.Running() {
		t.Fatal("engine should not be running after failed Start")
	}
	eng.Stop() // no-op on idle engine
}

func TestUpdateConfigRejectedWhileRunning(t *testing.T) {
	hub, addr := startTestHub(t)
	_ = hub

	eng := New(Config{
		Mode:         ModeTunnel,
		HubAddr:      addr,
		HubPlaintext: true,
		Label:        "tunnel-relay",
		Identity:     testIdentity,
		DisableXray:  true,
		ConfigDir:    t.TempDir(),
	}, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "tunnel online", func() bool {
		s := eng.Status()
		return s.Phase == PhaseOnline && s.Transport == relay.TransportTunnel && s.RelayID != ""
	})

	if err := eng.UpdateConfig(Config{}); err == nil {
		t.Fatal("expected UpdateConfig to fail while running")
	}

	s := eng.Status()
	if s.PublicHost != "127.0.0.1" || s.PublicPort == 0 {
		t.Fatalf("unexpected published endpoint %s:%d", s.PublicHost, s.PublicPort)
	}

	eng.Stop()
	if eng.Status().Phase != PhaseIdle {
		t.Fatalf("phase after stop = %q, want idle", eng.Status().Phase)
	}
	if eng.Running() {
		t.Fatal("engine still running after Stop")
	}

	if err := eng.UpdateConfig(Config{Mode: ModeDirect, BrokerURL: "http://localhost:1"}); err != nil {
		t.Fatalf("UpdateConfig after stop: %v", err)
	}
}

// hubRegistrar satisfies tunnel.Registrar with canned relay IDs.
type hubRegistrar struct {
	ids atomic.Uint64
}

func (h *hubRegistrar) Register(_ context.Context, req relay.RegisterRequest) (tunnel.RelayRegistration, error) {
	id := h.ids.Add(1)
	return tunnel.RelayRegistration{
		RelayID:    fmt.Sprintf("relay_hub_%d", id),
		PublicHost: req.PublicHost,
		PublicPort: req.PublicPort,
		ExpiresAt:  time.Now().Add(time.Minute),
	}, nil
}

func (h *hubRegistrar) Heartbeat(context.Context, string) error { return nil }

func startTestHub(t *testing.T) (*tunnel.Hub, string) {
	t.Helper()
	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	port := freePort(t)
	alloc, err := tunnel.NewPortAllocator(port, port)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}
	hub := &tunnel.Hub{
		ControlListener:   controlLn,
		PublicHost:        "127.0.0.1",
		PublicBindHost:    "127.0.0.1",
		Allocator:         alloc,
		Registrar:         &hubRegistrar{},
		HeartbeatInterval: 50 * time.Millisecond,
		HandshakeTimeout:  2 * time.Second,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Serve(ctx) }()
	t.Cleanup(cancel)
	return hub, controlLn.Addr().String()
}

// TestPrepareIdentityBackfillsSeed pins the migration path for identity.json
// files written before IdentitySeed existed: the engine generates a seed,
// reports it through OnIdentity for persistence, and keeps every existing
// field untouched.
func TestPrepareIdentityBackfillsSeed(t *testing.T) {
	legacy := testIdentity
	legacy.IdentitySeed = ""
	var persisted []Identity
	eng := New(Config{
		BrokerURL: "http://127.0.0.1:1",
		Mode:      ModeDirect,
		Identity:  legacy,
	}, Events{OnIdentity: func(id Identity) { persisted = append(persisted, id) }})

	got, err := eng.prepareIdentity(eng.cfg)
	if err != nil {
		t.Fatalf("prepareIdentity: %v", err)
	}
	if got.IdentitySeed == "" {
		t.Fatal("expected a generated identity seed")
	}
	if _, err := got.identityKey(); err != nil {
		t.Fatalf("generated seed does not parse: %v", err)
	}
	if got.ClientID != legacy.ClientID || got.RealityPublicKey != legacy.RealityPublicKey || got.ShortID != legacy.ShortID {
		t.Fatalf("existing identity fields changed: %+v", got)
	}
	if len(persisted) != 1 || persisted[0].IdentitySeed != got.IdentitySeed {
		t.Fatalf("expected the backfilled identity to be reported for persistence, got %+v", persisted)
	}

	// A fully populated identity is reported nowhere and stays byte-identical.
	persisted = nil
	again, err := eng.prepareIdentity(eng.cfg)
	if err != nil {
		t.Fatalf("prepareIdentity (second): %v", err)
	}
	if again != got || len(persisted) != 0 {
		t.Fatalf("stable identity must be a no-op: %+v (persisted %d)", again, len(persisted))
	}

	// A corrupt persisted seed refuses loudly instead of churning identity.
	corrupt := eng.cfg
	corrupt.Identity.IdentitySeed = "!!!not-base64!!!"
	if _, err := eng.prepareIdentity(corrupt); err == nil {
		t.Fatal("expected an error for a corrupt persisted seed")
	}
}
