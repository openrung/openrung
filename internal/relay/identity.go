package relay

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Relay identity (spec "openrung-relay-identity-v1").
//
// A relay may hold a long-lived Ed25519 identity keypair and prove possession
// of it at registration. The broker then derives the relay ID from the public
// key instead of minting a random one, so the same relay keeps the same ID
// across process restarts, broker restarts, and lease expiries — the churn
// that previously orphaned dashboard history and ranking state on every
// re-registration. Identity is optional: a registration without the identity
// fields keeps today's random-ID behavior, so old relays and hubs continue to
// work against a new broker (and new relays degrade gracefully against an old
// broker, which ignores the unknown fields).
//
// The proof is a signature over a canonical statement that binds the
// identity-relevant registration fields plus a relay-chosen expiry, so a
// captured proof cannot be replayed with altered content, after expiry, or
// across transports. The endpoint is bound only for direct transport: a
// tunnel relay cannot know the hub-assigned endpoint when it signs (the hub
// registers on its behalf and replays the stored request verbatim on broker
// loss, which is also why the proof carries an expiry instead of a nonce).
const (
	// IdentitySpecV1 is the domain-separation tag for both the signed
	// statement and the relay-ID derivation.
	IdentitySpecV1 = "openrung-relay-identity-v1"

	// MaxIdentityProofWindow bounds how far in the future a proof's expiry may
	// lie, capping replay of a captured proof. It must exceed both TTLs below
	// (a fast relay clock can date a proof up to its TTL ahead) with headroom.
	MaxIdentityProofWindow = 48 * time.Hour

	// The proof TTLs are the replay window for a captured registration, but a
	// replayed proof is low-value: it can only re-register the same identity —
	// a direct proof binds the endpoint (so it merely re-asserts the relay's
	// own registration), and a replayed tunnel proof cannot serve traffic
	// because the bound Reality public key's private half stays on the real
	// relay. So the TTLs are sized for clock tolerance, not minimized: a relay
	// whose clock is off by less than the TTL still registers. Direct signs a
	// fresh proof per call and tunnel re-signs on every reconnect (with
	// session recycling when the broker reports one expired), so a short-lived
	// proof is never a liability in practice.
	IdentityProofTTLDirect = 24 * time.Hour
	IdentityProofTTLTunnel = 24 * time.Hour
)

var (
	ErrIdentityIncomplete   = errors.New("identity_public_key, identity_proof, and identity_expires_at must be supplied together")
	ErrIdentityProofExpired = errors.New("relay identity proof expired")
	ErrIdentityProofInvalid = errors.New("invalid relay identity proof")
)

// IdentityStatement returns the exact bytes the relay signs. Fields are
// newline-joined in fixed order; every bound value is rejected elsewhere if it
// contains a newline (see ValidateIdentityFields), so the encoding is
// unambiguous. expiresAt is the verbatim identity_expires_at string from the
// request — the broker echoes the string into the statement and parses it
// separately for the freshness check, so no reformatting can desynchronize
// signer and verifier. The endpoint slots are bound only for direct transport;
// tunnel relays sign an empty host and port 0 because the hub assigns their
// endpoint after the fact. Transport is always bound, so a tunnel proof can
// never be replayed as a direct registration at an attacker-chosen endpoint.
// RelayVersion is deliberately unbound: it changes on every upgrade and is
// display-only.
func IdentityStatement(req RegisterRequest, expiresAt string) []byte {
	transport := req.Transport
	if strings.TrimSpace(transport) == "" {
		transport = TransportDirect
	}
	host := req.PublicHost
	port := req.PublicPort
	if transport == TransportTunnel {
		host = ""
		port = 0
	}
	nodeClass := req.NodeClass
	if strings.TrimSpace(nodeClass) == "" {
		nodeClass = NodeClassVolunteer
	}
	fields := []string{
		IdentitySpecV1,
		expiresAt,
		transport,
		host,
		strconv.Itoa(port),
		req.ClientID,
		req.RealityPublicKey,
		req.ShortID,
		req.ServerName,
		req.Flow,
		req.ExitMode,
		strconv.Itoa(req.MaxSessions),
		strconv.Itoa(req.MaxMbps),
		req.Label,
		nodeClass,
	}
	return []byte(strings.Join(fields, "\n"))
}

// ValidateIdentityFields rejects bound fields containing newline or carriage
// return, which would make the newline-joined statement ambiguous. Only
// identity-bearing registrations pay this check.
func ValidateIdentityFields(req RegisterRequest) error {
	for name, value := range map[string]string{
		"public_host":        req.PublicHost,
		"client_id":          req.ClientID,
		"reality_public_key": req.RealityPublicKey,
		"short_id":           req.ShortID,
		"server_name":        req.ServerName,
		"flow":               req.Flow,
		"exit_mode":          req.ExitMode,
		"label":              req.Label,
		"node_class":         req.NodeClass,
		"transport":          req.Transport,
	} {
		if strings.ContainsAny(value, "\n\r") {
			return fmt.Errorf("%s may not contain newline characters", name)
		}
	}
	return nil
}

// SignIdentity produces the identity fields for a registration request: the
// base64 raw public key, the base64 signature over the canonical statement,
// and the RFC 3339 expiry string that was bound into it.
func SignIdentity(priv ed25519.PrivateKey, req RegisterRequest, expiresAt time.Time) (publicKey, proof, expires string) {
	expires = expiresAt.UTC().Format(time.RFC3339)
	signature := ed25519.Sign(priv, IdentityStatement(req, expires))
	publicKey = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	proof = base64.StdEncoding.EncodeToString(signature)
	return publicKey, proof, expires
}

// VerifyIdentity checks a registration's identity proof and returns the raw
// public key on success. It enforces: all three fields present together, a
// well-formed 32-byte key and 64-byte signature, an expiry that is in the
// future but no further out than MaxIdentityProofWindow, newline-safe bound
// fields, and a valid signature over the canonical statement.
func VerifyIdentity(req RegisterRequest, now time.Time) (ed25519.PublicKey, error) {
	if req.IdentityPublicKey == "" && req.IdentityProof == "" && req.IdentityExpiresAt == "" {
		return nil, nil
	}
	if req.IdentityPublicKey == "" || req.IdentityProof == "" || req.IdentityExpiresAt == "" {
		return nil, ErrIdentityIncomplete
	}
	if err := ValidateIdentityFields(req); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrIdentityProofInvalid, err)
	}
	publicKey, err := base64.StdEncoding.DecodeString(req.IdentityPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: identity_public_key must be a base64 32-byte Ed25519 key", ErrIdentityProofInvalid)
	}
	signature, err := base64.StdEncoding.DecodeString(req.IdentityProof)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: identity_proof must be a base64 64-byte signature", ErrIdentityProofInvalid)
	}
	expires, err := time.Parse(time.RFC3339, req.IdentityExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("%w: identity_expires_at must be RFC 3339", ErrIdentityProofInvalid)
	}
	if !expires.After(now) {
		return nil, ErrIdentityProofExpired
	}
	if expires.After(now.Add(MaxIdentityProofWindow)) {
		return nil, fmt.Errorf("%w: identity_expires_at is more than %s in the future", ErrIdentityProofInvalid, MaxIdentityProofWindow)
	}
	if !ed25519.Verify(publicKey, IdentityStatement(req, req.IdentityExpiresAt), signature) {
		return nil, ErrIdentityProofInvalid
	}
	return ed25519.PublicKey(publicKey), nil
}

// DeriveRelayID maps an identity public key to its stable relay ID:
// "relay_" + lowercase hex of the first 16 bytes of SHA-256 over the
// domain-separation tag and the raw key. Same shape as the legacy random IDs
// (relay_ + 32 hex chars), so nothing downstream needs to distinguish them.
func DeriveRelayID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(append([]byte(IdentitySpecV1+":id:"), publicKey...))
	return "relay_" + hex.EncodeToString(sum[:16])
}

// ParseIdentitySeed decodes a base64 32-byte Ed25519 seed (the same encoding
// the broker uses for its relay-list signing key) into a private key.
func ParseIdentitySeed(encoded string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("identity seed must be base64")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity seed must decode to %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// EncodeIdentitySeed is the inverse of ParseIdentitySeed, used when persisting
// a generated identity.
func EncodeIdentitySeed(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Seed())
}
