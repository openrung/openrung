package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A foundation token is a self-contained posture: it forces foundation class
// and direct mode even when a configured hub would otherwise resolve to auto,
// so the credential can never enter a hub path.
func TestFoundationTokenPostureForcesFoundationDirect(t *testing.T) {
	cfg := Config{
		FoundationToken: "fnd-secret",
		BrokerURL:       "https://broker.test",
		Mode:            ModeAuto,
		HubAddr:         "hub.example:9443",
	}.withDefaults()
	effective, err := cfg.effectiveConfig()
	if err != nil {
		t.Fatalf("effectiveConfig: %v", err)
	}
	if effective.NodeClass != relay.NodeClassFoundation || effective.Mode != ModeDirect {
		t.Fatalf("posture = %q/%q, want foundation/direct", effective.NodeClass, effective.Mode)
	}

	// An unnormalized foundation spelling is accepted and normalized.
	spelled := cfg
	spelled.NodeClass = " Foundation "
	effective, err = spelled.effectiveConfig()
	if err != nil {
		t.Fatalf("effectiveConfig with spelled class: %v", err)
	}
	if effective.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node class = %q, want normalized foundation", effective.NodeClass)
	}

	// An explicit non-foundation class is a contradiction, not an override.
	conflicted := cfg
	conflicted.NodeClass = relay.NodeClassVolunteer
	if _, err := conflicted.effectiveConfig(); err == nil {
		t.Fatal("effectiveConfig() error = nil, want a node-class conflict error")
	}
}

func TestValidateFoundationRequiresDirectMode(t *testing.T) {
	base := Config{
		BrokerURL: "https://broker.openrung.org",
		NodeClass: relay.NodeClassFoundation,
		HubAddr:   "hub.example:9443",
	}

	for _, mode := range []string{ModeAuto, ModeTunnel} {
		t.Run(mode, func(t *testing.T) {
			cfg := base
			cfg.Mode = mode
			err := cfg.withDefaults().validate()
			if err == nil {
				t.Fatalf("validate() error = nil, want foundation %s rejection", mode)
			}
			if !strings.Contains(err.Error(), "requires direct mode") {
				t.Fatalf("validate() error = %v, want direct-mode explanation", err)
			}
		})
	}

	direct := base
	direct.Mode = ModeDirect
	if err := direct.withDefaults().validate(); err != nil {
		t.Fatalf("validate() rejected foundation direct mode: %v", err)
	}

	// Auto without a hub never probes and always degrades to direct, so it is
	// safe for a foundation posture (mirrors cmd/relay's hubless direct default).
	hubless := base
	hubless.Mode = ModeAuto
	hubless.HubAddr = ""
	if err := hubless.withDefaults().validate(); err != nil {
		t.Fatalf("validate() rejected foundation auto-without-hub: %v", err)
	}
}

func TestFoundationTokenIsBearerAndForcesSecureTransport(t *testing.T) {
	cfg := Config{FoundationToken: "fnd-secret", Token: "vol-token", BrokerURL: "https://broker.test"}
	bc := cfg.brokerClient()
	if bc.Token != "fnd-secret" {
		t.Fatalf("bearer = %q, want the foundation token (not the volunteer token)", bc.Token)
	}
	if !bc.RequireSecureTransport {
		t.Fatal("RequireSecureTransport = false, want true for a foundation token")
	}

	classOnly := Config{NodeClass: relay.NodeClassFoundation, Token: "vol-token", BrokerURL: "https://broker.test"}
	if bc := classOnly.brokerClient(); !bc.RequireSecureTransport || bc.Token != "vol-token" {
		t.Fatalf("class-only client = token %q secure %t, want vol-token/true", bc.Token, bc.RequireSecureTransport)
	}

	volunteer := Config{Token: "vol-token", BrokerURL: "http://broker.test"}
	if bc := volunteer.brokerClient(); bc.RequireSecureTransport {
		t.Fatal("RequireSecureTransport = true for a volunteer config, want false")
	}
}

// The foundation bearer must never travel over cleartext to a non-loopback
// broker: both registration and heartbeat are refused before any request is
// sent.
func TestFoundationRefusesPlaintextBroker(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusCreated, `{"id":"relay_x","node_class":"foundation"}`), nil
	})}
	cfg := Config{FoundationToken: "fnd-secret", BrokerURL: "http://broker.test", HTTPClient: client}
	broker := cfg.brokerClient()
	if _, err := broker.Register(context.Background(), relay.RegisterRequest{}); err == nil {
		t.Fatal("Register() error = nil, want a cleartext-broker refusal")
	}
	if err := broker.Heartbeat(context.Background(), "relay_x", ""); err == nil {
		t.Fatal("Heartbeat() error = nil, want a cleartext-broker refusal")
	}
	if sent.Load() != 0 {
		t.Fatalf("broker requests sent = %d, want 0 (must refuse before sending the token)", sent.Load())
	}
}

// The broker client the engine builds must use only the canonical
// registration/heartbeat routes and refuse every redirect, so the bearer can
// never be forwarded to another location.
func TestEngineBrokerUsesCanonicalRoutesAndRefusesRedirects(t *testing.T) {
	var redirected atomic.Int32
	var mu sync.Mutex
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/redirect-target" {
			redirected.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cfg := Config{BrokerURL: ts.URL, Token: "tok"}
	broker := cfg.brokerClient()
	if _, err := broker.Register(context.Background(), relay.RegisterRequest{}); err == nil || !strings.Contains(err.Error(), "refused redirect") {
		t.Fatalf("Register() error = %v, want a refused-redirect error", err)
	}
	if err := broker.Heartbeat(context.Background(), "relay_1", "lease"); err == nil || !strings.Contains(err.Error(), "refused redirect") {
		t.Fatalf("Heartbeat() error = %v, want a refused-redirect error", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target hits = %d, want 0", redirected.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/api/v1/relays/register", "/api/v1/relays/relay_1/heartbeat"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("request paths = %v, want exactly the canonical routes %v", paths, want)
	}
}

// The full-session invariant: a foundation-token engine configured with a hub
// and auto mode must register directly, present the foundation bearer, claim
// foundation class, and never send a single request to the hub.
func TestFoundationTokenSessionRegistersDirectWithoutTouchingHub(t *testing.T) {
	stubIPv6(t, "2001:db8::1", nil)

	var hubRequests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	var gotAuth atomic.Value
	var gotClass atomic.Value
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/relays/register" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		gotAuth.Store(r.Header.Get("Authorization"))
		var req relay.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotClass.Store(req.NodeClass)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(relay.RegisterResponse{Descriptor: relay.Descriptor{
			ID:         "relay_fnd",
			NodeClass:  relay.NodeClassFoundation,
			PublicHost: req.PublicHost,
			PublicPort: req.PublicPort,
		}})
	}))
	defer broker.Close()

	eng := New(Config{
		BrokerURL:       broker.URL, // loopback http is allowed under the secure-transport policy
		FoundationToken: "fnd-secret",
		Mode:            ModeAuto,
		HubAddr:         "hub.example:9443",
		HubHTTPURL:      hub.URL,
		Label:           "fnd-relay",
		ListenPort:      freePort(t),
		Identity:        testIdentity,
		DisableXray:     true,
		ConfigDir:       t.TempDir(),
	}, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "foundation relay online", func() bool {
		s := eng.Status()
		return s.Phase == PhaseOnline && s.Transport == relay.TransportDirect && s.RelayID == "relay_fnd"
	})
	if got := hubRequests.Load(); got != 0 {
		t.Fatalf("hub requests = %d, want 0 (foundation posture must never touch the hub)", got)
	}
	if auth, _ := gotAuth.Load().(string); auth != "Bearer fnd-secret" {
		t.Fatalf("Authorization = %q, want the foundation token as bearer", auth)
	}
	if class, _ := gotClass.Load().(string); class != relay.NodeClassFoundation {
		t.Fatalf("registered node_class = %q, want foundation", class)
	}
}

// The runtime path has its own guard: even a session whose config bypassed
// Start-time validation is rejected before the probe can send anything to the
// hub.
func TestRunSessionRejectsFoundationAutoBeforeProbe(t *testing.T) {
	var hubRequests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	eng := New(Config{
		BrokerURL:  "https://broker.test",
		NodeClass:  " Foundation ",
		Mode:       ModeAuto,
		HubAddr:    "hub.example:9443",
		HubHTTPURL: hub.URL,
		Token:      "foundation-secret",
		Identity:   testIdentity,
	}, Events{})
	err := eng.runSession(context.Background(), eng.cfg.brokerClient())
	if err == nil {
		t.Fatal("runSession() error = nil, want foundation auto-mode rejection")
	}
	if hubRequests.Load() != 0 {
		t.Fatalf("hub requests = %d, want 0", hubRequests.Load())
	}
}

// A broker that predates node_class silently drops the field; a foundation
// relay must refuse to serve mislabeled rather than run as volunteer-class.
func TestDirectSessionRejectsUnattestedFoundationClass(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"relay_new","public_host":"relay.example","public_port":443}`))
	}))
	defer broker.Close()

	raw := Config{
		BrokerURL:   broker.URL,
		NodeClass:   relay.NodeClassFoundation,
		Mode:        ModeDirect,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}.withDefaults()
	cfg, err := raw.effectiveConfig()
	if err != nil {
		t.Fatalf("effectiveConfig: %v", err)
	}
	eng := New(raw, Events{})
	err = eng.runDirectSession(context.Background(), cfg.brokerClient(), cfg, "fnd-relay", testIdentity, "127.0.0.1", directOnlyListenHost)
	if err == nil || !strings.Contains(err.Error(), "node_class") {
		t.Fatalf("runDirectSession() error = %v, want an unattested-node-class error", err)
	}
}
