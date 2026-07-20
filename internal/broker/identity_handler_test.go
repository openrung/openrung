package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openrung/internal/relay"
)

// TestRegisterHandlerIdentityFlow covers the HTTP surface of stable identity:
// a valid proof registers with the derived ID, re-registration keeps it, and
// a tampered or expired proof is a loud 401 — never a silent random-ID
// fallback. The expired 401 body is the exact string the relay hub matches to
// recycle a tunnel session, so it is pinned here.
func TestRegisterHandlerIdentityFlow(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	priv, err := relay.ParseIdentitySeed(identityStoreSeedA)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	register := func(req relay.RegisterRequest) *httptest.ResponseRecorder {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
		return recorder
	}
	signed := func() relay.RegisterRequest {
		req := validRegisterRequest()
		req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt =
			relay.SignIdentity(priv, req, time.Now().Add(relay.IdentityProofTTLDirect))
		return req
	}

	first := register(signed())
	if first.Code != http.StatusCreated {
		t.Fatalf("identity register: expected 201, got %d: %s", first.Code, first.Body.String())
	}
	var firstDesc relay.Descriptor
	if err := json.Unmarshal(first.Body.Bytes(), &firstDesc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}

	second := register(signed())
	if second.Code != http.StatusCreated {
		t.Fatalf("identity re-register: expected 201, got %d: %s", second.Code, second.Body.String())
	}
	var secondDesc relay.Descriptor
	if err := json.Unmarshal(second.Body.Bytes(), &secondDesc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if secondDesc.ID != firstDesc.ID {
		t.Fatalf("relay ID churned across HTTP re-registration: %s -> %s", firstDesc.ID, secondDesc.ID)
	}

	// The identity public key must not leak through the public descriptor JSON.
	if bytes.Contains(second.Body.Bytes(), []byte("identity")) {
		t.Fatalf("identity material leaked into the register response: %s", second.Body.String())
	}

	tampered := signed()
	tampered.Label = "evil-otter"
	if recorder := register(tampered); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tampered proof: expected 401, got %d: %s", recorder.Code, recorder.Body.String())
	}

	expired := validRegisterRequest()
	expired.IdentityPublicKey, expired.IdentityProof, expired.IdentityExpiresAt =
		relay.SignIdentity(priv, expired, time.Now().Add(-time.Minute))
	recorder := register(expired)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired proof: expected 401, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var apiErr relay.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if apiErr.Error != "relay identity proof expired" {
		t.Fatalf("the hub matches this message verbatim; got %q", apiErr.Error)
	}
}
