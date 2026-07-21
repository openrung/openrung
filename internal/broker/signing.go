package broker

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"openrung/internal/relay"
)

// Relay-list signing detaches directory authenticity from the transport, so
// discovery can safely use non-TLS channels (the direct-IP fallback, static
// mirrors). It defends channel integrity only: a compromised broker signs
// whatever it serves, and a censor can still block or inject errors — clients
// treat all of that as "candidate failed", never as forged data accepted.

const (
	// signatureHeader carries the detached body signature as
	// "ed25519;<key_id>;<base64 signature>". Only 2xx relay-list responses are
	// signed; error bodies (writeError) stay unsigned, and clients treat any
	// unsigned or non-2xx response as a failed candidate.
	signatureHeader = "X-OpenRung-Relays-Signature"

	// apiNotAfterWindow bounds replay of an API-channel response.
	// mirrorNotAfterWindow bounds the long-lived mirror artifacts, which a
	// cron republishes hourly, so 24 h is a safety margin rather than a
	// staleness budget.
	apiNotAfterWindow    = 30 * time.Minute
	mirrorNotAfterWindow = 24 * time.Hour
)

// signer signs relay-list response bodies with the broker's online Ed25519
// key. keyID ships in the header and body so clients can route to the right
// pinned public key without trial verification.
type signer struct {
	key   ed25519.PrivateKey
	keyID string
}

// newSigner derives the signing key from a 32-byte Ed25519 seed. Callers
// validate the seed with ParseSigningSeed at startup, so anything else here is
// a programming error: panic rather than silently serving unsigned lists,
// which verifying clients would reject while healthz stayed green.
func newSigner(seed []byte) signer {
	if len(seed) != ed25519.SeedSize {
		panic(fmt.Sprintf("broker: Config.SigningSeed must be %d bytes, got %d (validate with ParseSigningSeed)", ed25519.SeedSize, len(seed)))
	}
	key := ed25519.NewKeyFromSeed(seed)
	return signer{key: key, keyID: signingKeyID(key.Public().(ed25519.PublicKey))}
}

// signingKeyID returns the advisory key identifier: lowercase hex of the
// first 8 bytes of SHA-256 over the raw 32-byte public key.
func signingKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParseSigningSeed decodes and validates OPENRUNG_RELAY_SIGNING_KEY: standard
// base64 of the 32-byte Ed25519 signing seed. Any failure must abort startup —
// a crash loop is an ordinary, visible outage, whereas serving unsigned relay
// lists is the least detectable failure mode available (healthz and old
// clients stay green while every verifying client loses discovery).
func ParseSigningSeed(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("OPENRUNG_RELAY_SIGNING_KEY is empty: refusing to serve unsigned relay lists that verifying clients would reject. Set OPENRUNG_RELAY_SIGNING_KEY to the standard-base64 32-byte Ed25519 signing seed")
	}
	seed, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("OPENRUNG_RELAY_SIGNING_KEY is not valid standard base64: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("OPENRUNG_RELAY_SIGNING_KEY must decode to exactly %d bytes (the Ed25519 seed), got %d", ed25519.SeedSize, len(seed))
	}
	return seed, nil
}

// SigningKeyID reports the advisory key identifier for a validated signing
// seed, so cmd/broker can log the active key at startup without a signer.
func SigningKeyID(seed []byte) string {
	key := ed25519.NewKeyFromSeed(seed)
	return signingKeyID(key.Public().(ed25519.PublicKey))
}

// writeSigned marshals resp once, signs those exact bytes, and writes the same
// buffer — sign-what-you-send. Never route this through writeJSON: Encode
// appends a trailing newline that Marshal does not (an invisible one-byte
// verification killer), and headers could not be set after its first write.
func (s signer) writeSigned(w http.ResponseWriter, resp relay.ListResponse) {
	resp = normalizeSignedRelayListTimes(resp)
	body, err := json.Marshal(resp) // NOT Encode; no trailing newline
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not encode relay list")
		return
	}
	sig := ed25519.Sign(s.key, body) // sign the exact buffer that is written
	w.Header().Set("Content-Type", "application/json")
	// no-transform additionally deters benign middlebox recompression on the
	// cleartext direct-IP path, which would break byte-exact verification.
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set(signatureHeader, "ed25519;"+s.keyID+";"+base64.StdEncoding.EncodeToString(sig))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) // identical buffer — sign-what-you-send
}

// normalizeSignedRelayListTimes narrows the public directory wire format to
// UTC RFC 3339 at whole-second precision, which every supported client can
// decode. It clones the descriptor slice before changing timestamps so the
// broker retains subsecond precision for expiry checks and ranking.
func normalizeSignedRelayListTimes(resp relay.ListResponse) relay.ListResponse {
	resp.ServerTime = resp.ServerTime.UTC().Truncate(time.Second)
	resp.NotAfter = resp.NotAfter.UTC().Truncate(time.Second)
	if resp.Relays != nil {
		relays := make([]relay.Descriptor, len(resp.Relays))
		copy(relays, resp.Relays)
		for i := range relays {
			relays[i].RegisteredAt = relays[i].RegisteredAt.UTC().Truncate(time.Second)
			relays[i].LastHeartbeatAt = relays[i].LastHeartbeatAt.UTC().Truncate(time.Second)
			relays[i].ExpiresAt = relays[i].ExpiresAt.UTC().Truncate(time.Second)
		}
		resp.Relays = relays
	}
	return resp
}
