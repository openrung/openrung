package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// A foundation relay must complete broker attestation BEFORE it opens its
// public listener: if attestation fails, the public port must never accept
// traffic and run() must return without entering the heartbeat loop. The
// broker mock checks whether the public port is already listening at the
// moment it is asked to attest, which would mean the relay was serving first.
func TestRunFoundationAttestsBeforePublicListener(t *testing.T) {
	// Reserve an ephemeral port for the relay's public listener, then release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	publicPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	publicAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort))

	// A fake xray that just stays alive, so a (hypothetical) listener-first
	// ordering would actually start the observer and bind the public port.
	fakeXray := filepath.Join(t.TempDir(), "xray")
	if err := os.WriteFile(fakeXray, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}

	var registerCalls, heartbeatCalls atomic.Int32
	var portOpenAtAttest atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/heartbeat") {
			heartbeatCalls.Add(1)
			return jsonResponse(http.StatusOK, `{"ok":true}`), nil
		}
		registerCalls.Add(1)
		if conn, derr := net.DialTimeout("tcp", publicAddr, 200*time.Millisecond); derr == nil {
			portOpenAtAttest.Store(true)
			_ = conn.Close()
		}
		// Broker predates node_class (drops the field): attestation must fail.
		return jsonResponse(http.StatusCreated, `{"id":"relay_x","public_host":"127.0.0.1","public_port":`+strconv.Itoa(publicPort)+`}`), nil
	})}

	cfg := cliConfig{
		Mode:              "direct",
		BrokerURL:         "https://broker.test",
		PublicHost:        "127.0.0.1",
		PublicPort:        publicPort,
		ListenHost:        "127.0.0.1",
		ListenPort:        publicPort,
		ServerName:        "www.cloudflare.com",
		RealityDest:       "www.cloudflare.com:443",
		ClientID:          "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
		RealityPublicKey:  "pk",
		RealityPrivateKey: "sk",
		ShortID:           "5f7a8d9c",
		NodeClass:         relay.NodeClassFoundation,
		XrayPath:          fakeXray,
		ConnectionLog:     true,
		ConfigOut:         filepath.Join(t.TempDir(), "xray-config.json"),
		HeartbeatInterval: 30 * time.Second,
		MaxSessions:       1,
		MaxMbps:           1,
		HTTPClient:        client,
	}

	err = run(cfg)
	if err == nil {
		t.Fatal("run() error = nil, want foundation attestation failure")
	}
	if !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("run() error = %v, want a foundation attestation failure", err)
	}
	if registerCalls.Load() != 1 {
		t.Fatalf("register calls = %d, want exactly 1 (attestation)", registerCalls.Load())
	}
	if heartbeatCalls.Load() != 0 {
		t.Fatalf("heartbeat calls = %d, want 0 (must not reach the heartbeat loop)", heartbeatCalls.Load())
	}
	if portOpenAtAttest.Load() {
		t.Fatal("public listener was already accepting connections at attestation time; foundation must attest before serving")
	}
	if conn, derr := net.DialTimeout("tcp", publicAddr, 100*time.Millisecond); derr == nil {
		_ = conn.Close()
		t.Fatal("public port still listening after attestation failure; it must never have opened")
	}
}
