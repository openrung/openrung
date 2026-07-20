package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openrung/internal/relay"
)

// TestSignHelloSurvivesHubRegisterRequest pins the tunnel identity chain: the
// relay signs at HELLO time without knowing the hub-assigned endpoint, the hub
// builds its RegisterRequest (endpoint, exit host, punch settings, tunnel
// transport), and the broker-side verifier must accept the result. If the
// hub's registerRequest ever starts overwriting a statement-bound field, this
// fails.
func TestSignHelloSurvivesHubRegisterRequest(t *testing.T) {
	priv, err := relay.ParseIdentitySeed("QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	hello := HelloFrame{
		RealityPublicKey: "hSN7wJowfoOdmnbRDW9BC9BXGCyPTM6PqFOQqUFvvXo",
		ShortID:          "0123abcd",
		ServerName:       "www.cloudflare.com",
		ClientID:         "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		MaxSessions:      8,
		MaxMbps:          20,
		Label:            "witty-otter",
		RelayVersion:     "relay/1.0.0",
		StreamTyping:     true,
		PunchCapable:     true,
	}
	SignHello(priv, &hello, now.Add(relay.IdentityProofTTLTunnel))

	hub := &Hub{PublicHost: "203.0.113.99", PunchEndpoint: "https://203.0.113.99:9444"}
	req := hub.registerRequest(hello, 40007, "192.0.2.55")

	key, err := relay.VerifyIdentity(req, now)
	if err != nil {
		t.Fatalf("hub-shaped request failed verification: %v", err)
	}
	if key == nil {
		t.Fatal("identity fields were dropped between HELLO and RegisterRequest")
	}

	// The proof must stay valid on the hub's verbatim re-register later in the
	// session, and reject once the relay-chosen expiry passes.
	if _, err := relay.VerifyIdentity(req, now.Add(23*time.Hour)); err != nil {
		t.Fatalf("verbatim re-register within the proof TTL failed: %v", err)
	}
	if _, err := relay.VerifyIdentity(req, now.Add(25*time.Hour)); !errors.Is(err, relay.ErrIdentityProofExpired) {
		t.Fatalf("expected expiry after the proof TTL, got %v", err)
	}

	// A tunnel proof must not be replayable as a direct registration at an
	// attacker-chosen endpoint: transport is bound.
	hijack := req
	hijack.Transport = relay.TransportDirect
	if _, err := relay.VerifyIdentity(hijack, now); err == nil {
		t.Fatal("tunnel proof was accepted for a direct registration")
	}
}

// TestRegistrarMapsIdentityProofExpired pins the error contract the hub's
// session-recycling depends on.
func TestRegistrarMapsIdentityProofExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(relay.ErrorResponse{Error: "relay identity proof expired"})
	}))
	defer server.Close()

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	_, err := registrar.Register(context.Background(), relay.RegisterRequest{})
	if !errors.Is(err, ErrIdentityProofExpired) {
		t.Fatalf("expected ErrIdentityProofExpired, got %v", err)
	}
}

// TestClaimTunnelRejectsStaleSessionOnSharedID pins the fix for shared stable
// IDs: when a fast reconnect (session B) already holds the relay ID, the stale
// session A re-registering the SAME ID must not clobber B in the punch
// registry — claimTunnel refuses, and A is expected to retire.
func TestClaimTunnelRejectsStaleSessionOnSharedID(t *testing.T) {
	hub := &Hub{}
	a := &tunnel{}
	b := &tunnel{}
	const id = "relay_stable"

	// A connects first and installs itself.
	hub.addTunnel(id, a)
	if hub.lookupTunnel(id) != a {
		t.Fatal("A should own the slot after connect")
	}

	// B reconnects with the same derived ID and takes the slot (newest wins).
	hub.addTunnel(id, b)
	if hub.lookupTunnel(id) != b {
		t.Fatal("B should own the slot after its connect")
	}

	// A, now stale, re-registers the same ID after the broker forgot the lease.
	// It must lose the claim rather than overwrite B.
	if hub.claimTunnel(id, a) {
		t.Fatal("stale session A must not reclaim an ID owned by live session B")
	}
	if hub.lookupTunnel(id) != b {
		t.Fatal("claimTunnel by A corrupted B's registry entry")
	}

	// A's eventual teardown is a no-op (compare-and-delete by pointer).
	hub.removeTunnel(id, a)
	if hub.lookupTunnel(id) != b {
		t.Fatal("A's teardown evicted B")
	}

	// B may still refresh its own claim (the normal reregister path).
	if !hub.claimTunnel(id, b) {
		t.Fatal("the owning session must be able to reclaim its own ID")
	}
}
