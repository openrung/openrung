package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestHeartbeatOrRegisterRecoversForgottenRelay(t *testing.T) {
	var registrations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/volunteers/relay_old/heartbeat":
			return jsonResponse(http.StatusNotFound, `{"error":"relay not found"}`), nil
		case "/api/v1/volunteers/register":
			registrations.Add(1)
			return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{"error":"unexpected path"}`), nil
		}
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client}
	desc, reRegistered, err := heartbeatOrRegister(context.Background(), cfg, preparedRuntime{}, relay.Descriptor{ID: "relay_old"})
	if err != nil {
		t.Fatalf("heartbeatOrRegister() error = %v", err)
	}
	if !reRegistered {
		t.Fatal("heartbeatOrRegister() did not report re-registration")
	}
	if desc.ID != "relay_new" {
		t.Fatalf("heartbeatOrRegister() ID = %q, want relay_new", desc.ID)
	}
	if registrations.Load() != 1 {
		t.Fatalf("registrations = %d, want 1", registrations.Load())
	}
}

func TestHeartbeatOrRegisterDoesNotRegisterOnOtherErrors(t *testing.T) {
	var registrations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/volunteers/register" {
			registrations.Add(1)
		}
		return jsonResponse(http.StatusInternalServerError, `{"error":"temporary failure"}`), nil
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", HTTPClient: client}
	original := relay.Descriptor{ID: "relay_old"}
	desc, reRegistered, err := heartbeatOrRegister(context.Background(), cfg, preparedRuntime{}, original)
	if err == nil {
		t.Fatal("heartbeatOrRegister() error = nil, want an error")
	}
	if reRegistered {
		t.Fatal("heartbeatOrRegister() unexpectedly reported re-registration")
	}
	if desc.ID != original.ID {
		t.Fatalf("heartbeatOrRegister() ID = %q, want %q", desc.ID, original.ID)
	}
	if registrations.Load() != 0 {
		t.Fatalf("registrations = %d, want 0", registrations.Load())
	}
}

func TestTunnelModeRequiresHub(t *testing.T) {
	cfg := cliConfig{TunnelMode: true, MaxSessions: 1, MaxMbps: 1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when hub missing in tunnel mode")
	}

	cfg.HubAddr = "hub.example:9443"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected tunnel config to validate: %v", err)
	}
}

func TestValidateFoundationRequiresDirectMode(t *testing.T) {
	base := cliConfig{
		BrokerURL:         "https://broker.openrung.org",
		NodeClass:         relay.NodeClassFoundation,
		HubAddr:           "hub.example:9443",
		ListenPort:        443,
		PublicHost:        "relay.example",
		PublicPort:        443,
		MaxSessions:       1,
		MaxMbps:           1,
		HeartbeatInterval: 5 * time.Second,
		ConnectionLog:     true,
	}

	for _, mode := range []string{"auto", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			cfg := base
			cfg.Mode = mode
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want foundation %s rejection", mode)
			}
			if !strings.Contains(err.Error(), "requires direct mode") {
				t.Fatalf("Validate() error = %v, want direct-mode explanation", err)
			}
		})
	}

	direct := base
	direct.Mode = "direct"
	if err := direct.Validate(); err != nil {
		t.Fatalf("Validate() rejected foundation direct mode: %v", err)
	}
}

// run has its own fail-closed guard so a programmatic caller cannot bypass
// Validate and send the foundation token in auto mode's hub probe.
func TestRunRejectsFoundationAutoBeforeProbe(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	cfg := cliConfig{
		Mode:              "auto",
		NodeClass:         " Foundation ",
		HubAddr:           "hub.example:9443",
		RegistrationToken: "foundation-secret",
		HTTPClient:        client,
	}

	err := run(cfg)
	if err == nil {
		t.Fatal("run() error = nil, want foundation auto-mode rejection")
	}
	if requests.Load() != 0 {
		t.Fatalf("run() sent %d requests, want 0", requests.Load())
	}
}

func TestTunnelModeSkipsPublicHostDetection(t *testing.T) {
	cfg := cliConfig{TunnelMode: true, HubAddr: "hub.example:9443"}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults in tunnel mode: %v", err)
	}
	if cfg.PublicHost != "" {
		t.Fatalf("expected no public host in tunnel mode, got %q", cfg.PublicHost)
	}
	if cfg.Label == "" {
		t.Fatal("expected a generated label in tunnel mode")
	}
}

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

// A broker that predates node_class silently drops the field and returns a
// descriptor without it; a foundation relay must refuse to serve mislabeled
// rather than silently run as a volunteer.
func TestRegisterRejectsUnattestedFoundationClass(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443}`), nil
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	if _, err := register(context.Background(), cfg, preparedRuntime{}); err == nil {
		t.Fatal("register() error = nil, want an unattested-node-class error")
	}
}

func TestRegisterAcceptsAttestedFoundationClass(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443,"node_class":"foundation"}`), nil
	})}

	cfg := cliConfig{BrokerURL: "https://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	desc, err := register(context.Background(), cfg, preparedRuntime{})
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want foundation", desc.NodeClass)
	}
}

// register() must refuse a foundation registration over a cleartext broker URL
// before it ever sends the token (enforced by BrokerClient.RequireSecureTransport,
// which brokerClient() sets for foundation).
func TestRegisterRefusesFoundationOverPlaintext(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	if _, err := register(context.Background(), cfg, preparedRuntime{}); err == nil {
		t.Fatal("register() error = nil, want a cleartext-broker error")
	}
	if sent.Load() != 0 {
		t.Fatalf("register sent %d requests, want 0 (must refuse before sending the token)", sent.Load())
	}
}

// Heartbeats carry the same foundation bearer as registration, so the secure
// transport policy must cover them too.
func TestHeartbeatRefusesFoundationOverPlaintext(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	cfg := cliConfig{
		BrokerURL:         "http://broker.test",
		RegistrationToken: "foundation-secret",
		HTTPClient:        client,
		NodeClass:         relay.NodeClassFoundation,
	}
	if err := heartbeat(context.Background(), cfg, "relay_foundation"); err == nil {
		t.Fatal("heartbeat() error = nil, want a cleartext-broker error")
	}
	if sent.Load() != 0 {
		t.Fatalf("heartbeat sent %d requests, want 0", sent.Load())
	}
}

// A foundation token is a self-contained posture: setting only
// OPENRUNG_FOUNDATION_TOKEN (no OPENRUNG_NODE_CLASS) must register as
// foundation, over the foundation bearer, on an https broker.
func TestFoundationTokenRegistersAsFoundationWithoutNodeClass(t *testing.T) {
	var gotAuth, gotBody string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}
		return jsonResponse(http.StatusCreated, `{"id":"relay_x","public_host":"relay.example","public_port":443,"node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{
		BrokerURL:       "https://broker.test",
		FoundationToken: "fnd-secret",
		PublicHost:      "relay.example",
		PublicPort:      443,
		HTTPClient:      client,
	}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if cfg.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want foundation (forced by the token)", cfg.NodeClass)
	}
	if cfg.Mode != "direct" {
		t.Fatalf("mode = %q, want direct (forced by the token)", cfg.Mode)
	}
	desc, err := register(context.Background(), cfg, preparedRuntime{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("attested node_class = %q, want foundation", desc.NodeClass)
	}
	if gotAuth != "Bearer fnd-secret" {
		t.Fatalf("Authorization = %q, want the foundation token as bearer", gotAuth)
	}
	if !strings.Contains(gotBody, `"node_class":"foundation"`) {
		t.Fatalf("register body did not claim foundation: %s", gotBody)
	}
}

// A hub var would normally resolve to auto; a foundation token overrides that
// to direct so the token can never route through a hub.
func TestFoundationTokenForcesDirectOverResolvedAuto(t *testing.T) {
	cfg := cliConfig{
		FoundationToken: "fnd-secret",
		BrokerURL:       "https://broker.test",
		PublicHost:      "relay.example",
		HubAddr:         "hub.example:9443",
	}
	cfg.Mode = normalizeMode(cfg.Mode, cfg.TunnelMode, cfg.HubAddr) // main() does this first
	if cfg.Mode != "auto" {
		t.Fatalf("precondition: mode = %q, want auto (hub implies auto)", cfg.Mode)
	}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if cfg.NodeClass != relay.NodeClassFoundation || cfg.Mode != "direct" {
		t.Fatalf("posture = %q/%q, want foundation/direct", cfg.NodeClass, cfg.Mode)
	}
}

func TestFoundationTokenConflictsWithExplicitVolunteerClass(t *testing.T) {
	cfg := cliConfig{FoundationToken: "fnd-secret", NodeClass: "volunteer", BrokerURL: "https://broker.test", PublicHost: "relay.example"}
	if err := cfg.ApplyDefaults(); err == nil {
		t.Fatal("ApplyDefaults() error = nil, want a node-class conflict error")
	}
}

func TestFoundationTokenIsBearerAndForcesSecureTransport(t *testing.T) {
	cfg := cliConfig{FoundationToken: "fnd-secret", RegistrationToken: "vol-token", BrokerURL: "https://broker.test"}
	bc := cfg.brokerClient()
	if bc.Token != "fnd-secret" {
		t.Fatalf("bearer = %q, want the foundation token (not the volunteer token)", bc.Token)
	}
	if !bc.RequireSecureTransport {
		t.Fatal("RequireSecureTransport = false, want true for a foundation token")
	}
}

// The token forces secure transport intrinsically: a foundation token against a
// cleartext broker is refused before any request is sent.
func TestFoundationTokenRefusesPlaintextBroker(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusCreated, `{"node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{FoundationToken: "fnd-secret", BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if _, err := register(context.Background(), cfg, preparedRuntime{}); err == nil {
		t.Fatal("register() error = nil, want a cleartext-broker refusal")
	}
	if sent.Load() != 0 {
		t.Fatalf("register sent %d requests, want 0 (must refuse before sending)", sent.Load())
	}
}
