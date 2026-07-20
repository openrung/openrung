package relay

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func identityTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv, err := ParseIdentitySeed("QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	if err != nil {
		t.Fatalf("parse test seed: %v", err)
	}
	return priv
}

func identityTestRequest() RegisterRequest {
	return RegisterRequest{
		PublicHost:       "203.0.113.7",
		PublicPort:       443,
		Protocol:         ProtocolVLESSRealityVision,
		ClientID:         "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		RealityPublicKey: "hSN7wJowfoOdmnbRDW9BC9BXGCyPTM6PqFOQqUFvvXo",
		ShortID:          "0123abcd",
		ServerName:       "www.cloudflare.com",
		Flow:             FlowVision,
		ExitMode:         ExitModeDirect,
		MaxSessions:      8,
		MaxMbps:          20,
		Label:            "witty-otter",
	}
}

// TestIdentityGoldenVectors pins the wire format: statement bytes, derived
// relay ID, and the (deterministic) Ed25519 proof. Breaking these breaks every
// deployed relay and hub, so change them only with a new spec version.
func TestIdentityGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/identity_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors struct {
		IdentitySeedBase64      string `json:"identity_seed_base64"`
		IdentityPublicKeyBase64 string `json:"identity_public_key_base64"`
		DerivedRelayID          string `json:"derived_relay_id"`
		DirectRegistration      struct {
			IdentityExpiresAt   string `json:"identity_expires_at"`
			Statement           string `json:"statement"`
			IdentityProofBase64 string `json:"identity_proof_base64"`
		} `json:"direct_registration"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}

	priv, err := ParseIdentitySeed(vectors.IdentitySeedBase64)
	if err != nil {
		t.Fatalf("parse vector seed: %v", err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	if got := base64.StdEncoding.EncodeToString(pub); got != vectors.IdentityPublicKeyBase64 {
		t.Fatalf("public key mismatch: got %s want %s", got, vectors.IdentityPublicKeyBase64)
	}
	if got := DeriveRelayID(pub); got != vectors.DerivedRelayID {
		t.Fatalf("derived relay ID mismatch: got %s want %s", got, vectors.DerivedRelayID)
	}

	req := identityTestRequest()
	if got := string(IdentityStatement(req, vectors.DirectRegistration.IdentityExpiresAt)); got != vectors.DirectRegistration.Statement {
		t.Fatalf("statement mismatch:\n got %q\nwant %q", got, vectors.DirectRegistration.Statement)
	}
	expires, err := time.Parse(time.RFC3339, vectors.DirectRegistration.IdentityExpiresAt)
	if err != nil {
		t.Fatalf("parse vector expiry: %v", err)
	}
	_, proof, expiresOut := SignIdentity(priv, req, expires)
	if expiresOut != vectors.DirectRegistration.IdentityExpiresAt {
		t.Fatalf("expiry serialization mismatch: got %s", expiresOut)
	}
	if proof != vectors.DirectRegistration.IdentityProofBase64 {
		t.Fatalf("proof mismatch:\n got %s\nwant %s", proof, vectors.DirectRegistration.IdentityProofBase64)
	}
}

func TestVerifyIdentityRoundTrip(t *testing.T) {
	priv := identityTestKey(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	req := identityTestRequest()
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = SignIdentity(priv, req, now.Add(time.Hour))

	key, err := VerifyIdentity(req, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if DeriveRelayID(key) != DeriveRelayID(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("verified key does not match the signer")
	}

	// A request with no identity fields is a legacy registration: nil key, no error.
	legacy := identityTestRequest()
	key, err = VerifyIdentity(legacy, now)
	if err != nil || key != nil {
		t.Fatalf("legacy request must verify to nil key: key=%v err=%v", key, err)
	}
}

// TestVerifyIdentityBindsEveryStatementField flips each bound field after
// signing and requires verification to fail: a captured proof must not be
// replayable with altered registration content.
func TestVerifyIdentityBindsEveryStatementField(t *testing.T) {
	priv := identityTestKey(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	mutations := map[string]func(*RegisterRequest){
		"public_host":        func(r *RegisterRequest) { r.PublicHost = "198.51.100.99" },
		"public_port":        func(r *RegisterRequest) { r.PublicPort = 8443 },
		"transport":          func(r *RegisterRequest) { r.Transport = TransportTunnel },
		"client_id":          func(r *RegisterRequest) { r.ClientID = "d2719f3a-0000-4562-b3fc-2c963f66afa6" },
		"reality_public_key": func(r *RegisterRequest) { r.RealityPublicKey = "AAAAwJowfoOdmnbRDW9BC9BXGCyPTM6PqFOQqUFvvXo" },
		"short_id":           func(r *RegisterRequest) { r.ShortID = "ffffffff" },
		"server_name":        func(r *RegisterRequest) { r.ServerName = "www.example.com" },
		"flow":               func(r *RegisterRequest) { r.Flow = "none" },
		"exit_mode":          func(r *RegisterRequest) { r.ExitMode = ExitModeDedicated },
		"max_sessions":       func(r *RegisterRequest) { r.MaxSessions = 999 },
		"max_mbps":           func(r *RegisterRequest) { r.MaxMbps = 999 },
		"label":              func(r *RegisterRequest) { r.Label = "evil-otter" },
		"node_class":         func(r *RegisterRequest) { r.NodeClass = NodeClassFoundation },
		"expires_at":         func(r *RegisterRequest) { r.IdentityExpiresAt = now.Add(30 * time.Hour).UTC().Format(time.RFC3339) },
	}
	for field, mutate := range mutations {
		req := identityTestRequest()
		req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = SignIdentity(priv, req, now.Add(time.Hour))
		mutate(&req)
		if _, err := VerifyIdentity(req, now); err == nil {
			t.Errorf("mutating %s after signing must fail verification", field)
		}
	}

	// Unbound fields may change without breaking the proof: the hub sets them
	// after the relay signed (tunnel endpoint, punch settings, exit host), and
	// relay_version changes on every upgrade.
	req := identityTestRequest()
	req.Transport = TransportTunnel
	req.PublicHost = ""
	req.PublicPort = 0
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = SignIdentity(priv, req, now.Add(time.Hour))
	req.PublicHost = "hub.example.org"
	req.PublicPort = 40001
	req.ExitHost = "192.0.2.55"
	req.PunchCapable = true
	req.PunchEndpoint = "https://hub.example.org:9444"
	req.RelayVersion = "relay/9.9.9"
	if _, err := VerifyIdentity(req, now); err != nil {
		t.Fatalf("hub-controlled fields must stay outside the tunnel statement: %v", err)
	}
}

func TestVerifyIdentityExpiryWindow(t *testing.T) {
	priv := identityTestKey(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	expired := identityTestRequest()
	expired.IdentityPublicKey, expired.IdentityProof, expired.IdentityExpiresAt = SignIdentity(priv, expired, now.Add(-time.Second))
	if _, err := VerifyIdentity(expired, now); !errors.Is(err, ErrIdentityProofExpired) {
		t.Fatalf("expected ErrIdentityProofExpired, got %v", err)
	}

	tooFar := identityTestRequest()
	tooFar.IdentityPublicKey, tooFar.IdentityProof, tooFar.IdentityExpiresAt = SignIdentity(priv, tooFar, now.Add(MaxIdentityProofWindow+time.Hour))
	if _, err := VerifyIdentity(tooFar, now); !errors.Is(err, ErrIdentityProofInvalid) {
		t.Fatalf("expected ErrIdentityProofInvalid for an over-window expiry, got %v", err)
	}
}

func TestVerifyIdentityRejectsPartialFieldsAndNewlines(t *testing.T) {
	priv := identityTestKey(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	partial := identityTestRequest()
	partial.IdentityPublicKey, partial.IdentityProof, partial.IdentityExpiresAt = SignIdentity(priv, partial, now.Add(time.Hour))
	partial.IdentityProof = ""
	if _, err := VerifyIdentity(partial, now); !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("expected ErrIdentityIncomplete, got %v", err)
	}

	// A newline inside a bound field would let two different field sets
	// produce one statement; identity registrations refuse it outright.
	sneaky := identityTestRequest()
	sneaky.Label = "witty-otter\nvolunteer"
	sneaky.IdentityPublicKey, sneaky.IdentityProof, sneaky.IdentityExpiresAt = SignIdentity(priv, sneaky, now.Add(time.Hour))
	if _, err := VerifyIdentity(sneaky, now); !errors.Is(err, ErrIdentityProofInvalid) {
		t.Fatalf("expected newline rejection, got %v", err)
	}
}

func TestVerifyIdentityRejectsWrongKeyOrGarbage(t *testing.T) {
	priv := identityTestKey(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// Signature by one key presented with a different public key: the classic
	// hijack — knowing a relay's public identity (it is public) must not allow
	// registering as it.
	otherSeed := strings.Repeat("A", 43) + "="
	otherPriv, err := ParseIdentitySeed(otherSeed)
	if err != nil {
		t.Fatalf("parse other seed: %v", err)
	}
	req := identityTestRequest()
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = SignIdentity(otherPriv, req, now.Add(time.Hour))
	req.IdentityPublicKey = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if _, err := VerifyIdentity(req, now); !errors.Is(err, ErrIdentityProofInvalid) {
		t.Fatalf("expected forged-key rejection, got %v", err)
	}

	malformed := identityTestRequest()
	malformed.IdentityPublicKey = "not-base64!"
	malformed.IdentityProof = "AAAA"
	malformed.IdentityExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
	if _, err := VerifyIdentity(malformed, now); !errors.Is(err, ErrIdentityProofInvalid) {
		t.Fatalf("expected malformed-key rejection, got %v", err)
	}
}

func TestParseIdentitySeed(t *testing.T) {
	if _, err := ParseIdentitySeed("!!!"); err == nil {
		t.Fatal("expected error for non-base64 seed")
	}
	if _, err := ParseIdentitySeed(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected error for wrong-length seed")
	}
	priv := identityTestKey(t)
	round, err := ParseIdentitySeed(EncodeIdentitySeed(priv))
	if err != nil || !round.Equal(priv) {
		t.Fatalf("seed round-trip failed: %v", err)
	}
}
