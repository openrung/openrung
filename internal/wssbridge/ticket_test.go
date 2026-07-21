package wssbridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T, now time.Time) (*TicketSigner, ed25519.PrivateKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewTicketSigner(key, TicketOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return signer, key
}

func validClaims(now time.Time, relayID, frontID, jti string) Claims {
	seconds := now.UTC().Truncate(time.Second).Unix()
	return Claims{
		Version: TicketVersion, Audience: TicketAudience,
		JTI: jti, RelayID: relayID, FrontID: frontID,
		IssuedAt: seconds, NotBefore: seconds, ExpiresAt: seconds + 120,
		MaxStreams: 8,
	}
}

func TestTicketClaimsAreTargetFreeAndRelayBound(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	signer, key := testSigner(t, now)
	claims := validClaims(now, "relay-a", "front-a", "ticket-jti-00000001")
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"v", "aud", "jti", "relay_id", "front_id", "iat", "nbf", "exp", "max_streams"}
	if len(raw) != len(wantKeys) {
		t.Fatalf("claim keys = %v", raw)
	}
	for _, key := range wantKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing claim %q", key)
		}
	}
	if strings.Contains(string(payload), "target") || strings.Contains(string(payload), "host") || strings.Contains(string(payload), "port") {
		t.Fatalf("ticket leaked dial authority: %s", payload)
	}

	verifier, err := NewTicketVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): key.Public().(ed25519.PublicKey),
	}, "relay-a", TicketOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifier.Verify(token)
	if err != nil || got != claims {
		t.Fatalf("verify = %+v, %v", got, err)
	}

	wrongRelay, err := NewTicketVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): key.Public().(ed25519.PublicKey),
	}, "relay-b", TicketOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongRelay.Verify(token); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("wrong relay verification = %v", err)
	}
}

func TestTicketVerifierSupportsOverlappingKeyRotation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	active, activeKey := testSigner(t, now)
	standby, standbyKey := testSigner(t, now)
	verifier, err := NewTicketVerifier(map[string]ed25519.PublicKey{
		active.KeyID():  activeKey.Public().(ed25519.PublicKey),
		standby.KeyID(): standbyKey.Public().(ed25519.PublicKey),
	}, "relay-a", TicketOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i, signer := range []*TicketSigner{active, standby} {
		claims := validClaims(now, "relay-a", "front-a", "rotation-ticket-0000"+string(rune('1'+i)))
		token, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(token); err != nil {
			t.Fatalf("key %d did not verify: %v", i, err)
		}
	}
}

func TestTicketRejectsNonCanonicalOrExpiredClaims(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	signer, key := testSigner(t, now)
	verifier, err := NewTicketVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): key.Public().(ed25519.PublicKey),
	}, "relay-a", TicketOptions{Now: func() time.Time { return now.Add(10 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(validClaims(now, "relay-a", "front-a", "expired-ticket-0001"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(token); !errors.Is(err, ErrExpiredTicket) {
		t.Fatalf("expired ticket = %v", err)
	}
}
