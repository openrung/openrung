package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

// wssTestFronts returns a deliberately unsorted front list so tests observe
// the canonicalization the engine must apply before signing.
func wssTestFronts() []relay.WSSFrontDescriptor {
	return []relay.WSSFrontDescriptor{
		{ID: "front-b", URL: "wss://cdn-b.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
		{ID: "front-a", URL: "wss://cdn-a.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
	}
}

func testIdentityKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := relay.ParseIdentitySeed(testIdentity.IdentitySeed)
	if err != nil {
		t.Fatalf("parse test identity seed: %v", err)
	}
	return key
}

func validWSSConfig(brokerURL string) Config {
	return Config{
		BrokerURL:       brokerURL,
		FoundationToken: "fnd-secret",
		Mode:            ModeDirect,
		WSSFronts:       wssTestFronts(),
		ListenPort:      443,
		Identity:        testIdentity,
		DisableXray:     true,
	}
}

// The gate invariant: a non-foundation relay configured with WSS fronts fails
// fast — at Start and again on the runtime path — rather than registering.
func TestWSSFrontsRequireFoundationClassFailFast(t *testing.T) {
	var brokerRequests atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokerRequests.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"relay_x"}`))
	}))
	defer broker.Close()

	for name, nodeClass := range map[string]string{"unstated": "", "volunteer": relay.NodeClassVolunteer} {
		t.Run(name, func(t *testing.T) {
			cfg := validWSSConfig(broker.URL)
			cfg.FoundationToken = ""
			cfg.NodeClass = nodeClass
			eng := New(cfg, Events{})
			if err := eng.Start(); err == nil || !strings.Contains(err.Error(), "foundation") {
				eng.Stop()
				t.Fatalf("Start() error = %v, want a foundation-class rejection", err)
			}

			// New never validates, so exercise the runtime-path guard directly:
			// the session must fail before a single broker request.
			if err := eng.runSession(context.Background(), eng.cfg.brokerClient()); err == nil {
				t.Fatal("runSession() error = nil, want a foundation-class rejection")
			}
			if brokerRequests.Load() != 0 {
				t.Fatalf("broker requests = %d, want 0 (must fail fast, not register)", brokerRequests.Load())
			}
		})
	}
}

func TestWSSValidationGates(t *testing.T) {
	base := Config{
		BrokerURL:  "https://broker.openrung.org",
		NodeClass:  relay.NodeClassFoundation,
		Mode:       ModeDirect,
		WSSFronts:  wssTestFronts(),
		ListenPort: 443,
		Identity:   testIdentity,
	}
	if err := base.withDefaults().validate(); err != nil {
		t.Fatalf("valid WSS config rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"volunteer class":    func(c *Config) { c.NodeClass = relay.NodeClassVolunteer },
		"tunnel mode":        func(c *Config) { c.Mode, c.HubAddr = ModeTunnel, "hub.example:9443" },
		"auto mode with hub": func(c *Config) { c.Mode, c.HubAddr = ModeAuto, "hub.example:9443" },
		"non-443 port":       func(c *Config) { c.ListenPort = 8443 },
		"connection logging": func(c *Config) { c.ConnectionLogOutput = &syncBuffer{} },
		"ephemeral identity": func(c *Config) { c.Identity.IdentitySeed = "" },
		"invalid identity":   func(c *Config) { c.Identity.IdentitySeed = "not-base64" },
		"malformed front": func(c *Config) {
			c.WSSFronts = []relay.WSSFrontDescriptor{{ID: "front-a", URL: "https://cdn.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.withDefaults().validate(); err == nil {
				t.Fatal("validate() error = nil, want WSS posture rejection")
			}
		})
	}
}

// The full session: the engine canonicalizes the fronts, signs identity and
// WSS capability with matching expiries such that broker-side verification
// passes, and goes online only after the broker echoes the exact fronts under
// the stable relay ID.
func TestWSSSessionSignsCapabilityAndVerifiesEcho(t *testing.T) {
	stubIPv6(t, "2001:db8::1", nil)
	expectedRelayID := relay.DeriveRelayID(testIdentityKey(t).Public().(ed25519.PublicKey))

	var mu sync.Mutex
	var received relay.RegisterRequest
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/relays/register" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var req relay.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode registration: %v", err)
		}
		if err := relay.VerifyWSSCapability(req, time.Now()); err != nil {
			t.Errorf("broker-side capability verification failed: %v", err)
		}
		mu.Lock()
		received = req
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(relay.RegisterResponse{Descriptor: relay.Descriptor{
			ID:         expectedRelayID,
			NodeClass:  relay.NodeClassFoundation,
			WSSFronts:  slices.Clone(req.WSSFronts),
			PublicHost: req.PublicHost,
			PublicPort: req.PublicPort,
		}})
	}))
	defer broker.Close()

	cfg := validWSSConfig(broker.URL)
	cfg.ConfigDir = t.TempDir()
	eng := New(cfg, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "WSS relay online", func() bool {
		s := eng.Status()
		return s.Phase == PhaseOnline && s.RelayID == expectedRelayID
	})

	mu.Lock()
	defer mu.Unlock()
	if len(received.WSSFronts) != 2 || received.WSSFronts[0].ID != "front-a" || received.WSSFronts[1].ID != "front-b" {
		t.Fatalf("registration fronts not canonical: %#v", received.WSSFronts)
	}
	if received.WSSCapabilityProof == "" || received.WSSCapabilityExpiresAt != received.IdentityExpiresAt {
		t.Fatalf("capability proof fields = proof:%t expiry:%q identity-expiry:%q",
			received.WSSCapabilityProof != "", received.WSSCapabilityExpiresAt, received.IdentityExpiresAt)
	}
	if received.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("registered node_class = %q, want foundation", received.NodeClass)
	}
}

// Fail-closed on broker echo tampering: dropped fronts, a rewritten front, or
// a relay ID that does not match the stable identity all abort the session.
func TestWSSSessionRejectsBrokerEchoTampering(t *testing.T) {
	expectedRelayID := relay.DeriveRelayID(testIdentityKey(t).Public().(ed25519.PublicKey))
	canonical, err := relay.NormalizeWSSFronts(wssTestFronts())
	if err != nil {
		t.Fatal(err)
	}

	for name, responseDescriptor := range map[string]relay.Descriptor{
		"dropped fronts": {
			ID: expectedRelayID, NodeClass: relay.NodeClassFoundation,
		},
		"wrong relay ID": {
			ID: "relay_wrong", NodeClass: relay.NodeClassFoundation,
			WSSFronts: slices.Clone(canonical),
		},
		"rewritten front": {
			ID: expectedRelayID, NodeClass: relay.NodeClassFoundation,
			WSSFronts: []relay.WSSFrontDescriptor{
				{ID: "front-a", URL: "wss://other.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
				{ID: "front-b", URL: "wss://cdn-b.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(relay.RegisterResponse{Descriptor: responseDescriptor})
			}))
			defer broker.Close()

			raw := validWSSConfig(broker.URL)
			raw.ConfigDir = t.TempDir()
			raw = raw.withDefaults()
			cfg, err := raw.effectiveConfig()
			if err != nil {
				t.Fatalf("effectiveConfig: %v", err)
			}
			eng := New(raw, Events{})
			err = eng.runDirectSession(context.Background(), cfg.brokerClient(), cfg, "wss-relay", testIdentity, "2001:db8::1", directOnlyListenHost)
			if err == nil {
				t.Fatal("runDirectSession() error = nil, want fail-closed broker echo rejection")
			}
		})
	}
}

// A WSS registration must use exactly the explicit stable seed: a session
// whose prepared identity diverges from the configured seed is refused before
// registration.
func TestWSSSessionRejectsIdentityMismatch(t *testing.T) {
	var brokerRequests atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokerRequests.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"relay_x"}`))
	}))
	defer broker.Close()

	raw := validWSSConfig(broker.URL).withDefaults()
	raw.ConfigDir = t.TempDir()
	cfg, err := raw.effectiveConfig()
	if err != nil {
		t.Fatalf("effectiveConfig: %v", err)
	}
	// A healed/regenerated identity: same everything except a different seed.
	divergent := testIdentity
	divergent.IdentitySeed = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="
	eng := New(raw, Events{})
	err = eng.runDirectSession(context.Background(), cfg.brokerClient(), cfg, "wss-relay", divergent, "2001:db8::1", directOnlyListenHost)
	if err == nil || !strings.Contains(err.Error(), "stable identity seed") {
		t.Fatalf("runDirectSession() error = %v, want an identity-mismatch rejection", err)
	}
	if brokerRequests.Load() != 0 {
		t.Fatalf("broker requests = %d, want 0", brokerRequests.Load())
	}
}

func TestVerifyRegisteredDescriptor(t *testing.T) {
	identityKey := testIdentityKey(t)
	stableID := relay.DeriveRelayID(identityKey.Public().(ed25519.PublicKey))
	canonical, err := relay.NormalizeWSSFronts(wssTestFronts())
	if err != nil {
		t.Fatal(err)
	}

	volunteerReq := relay.RegisterRequest{}
	if err := verifyRegisteredDescriptor(volunteerReq, relay.Descriptor{ID: "relay_1"}, identityKey); err != nil {
		t.Fatalf("plain volunteer registration rejected: %v", err)
	}

	foundationReq := relay.RegisterRequest{NodeClass: relay.NodeClassFoundation}
	if err := verifyRegisteredDescriptor(foundationReq, relay.Descriptor{ID: "relay_1"}, identityKey); err == nil {
		t.Fatal("unattested foundation class accepted")
	}
	if err := verifyRegisteredDescriptor(foundationReq, relay.Descriptor{ID: "relay_1", NodeClass: relay.NodeClassFoundation}, identityKey); err != nil {
		t.Fatalf("attested foundation registration rejected: %v", err)
	}

	wssReq := relay.RegisterRequest{NodeClass: relay.NodeClassFoundation, WSSFronts: canonical}
	good := relay.Descriptor{ID: stableID, NodeClass: relay.NodeClassFoundation, WSSFronts: slices.Clone(canonical)}
	if err := verifyRegisteredDescriptor(wssReq, good, identityKey); err != nil {
		t.Fatalf("exact WSS echo rejected: %v", err)
	}
	if err := verifyRegisteredDescriptor(wssReq, relay.Descriptor{ID: stableID, NodeClass: relay.NodeClassFoundation}, identityKey); err == nil {
		t.Fatal("dropped WSS fronts accepted")
	}
	wrongID := good
	wrongID.ID = "relay_wrong"
	if err := verifyRegisteredDescriptor(wssReq, wrongID, identityKey); err == nil {
		t.Fatal("wrong relay ID accepted for a WSS registration")
	}
}
