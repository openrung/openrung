package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

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

	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	desc, err := register(context.Background(), cfg, preparedRuntime{})
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want foundation", desc.NodeClass)
	}
}
