// Relay-list signature verification (relay-list signing SPEC v1, client side).
//
// The broker signs the exact raw bytes of every 2xx /api/v1/relays response
// with an online Ed25519 key and ships the signature in the
// X-OpenRung-Relays-Signature header, so discovery can trust the list on any
// transport — CDN fronts, a future direct-IP fallback, static mirrors —
// without leaning on TLS. Verification therefore lives HERE, in the byte-level
// HTTP shim below the discovery layer: both desktop entry points
// (BrokerClient.ListRelays and desktop/discovery.ListRelays) feed their
// responses through ReadVerifiedRelayList so there is exactly one
// verification path.
//
// Signing defends channel integrity only. A censor can still block requests
// or strip the header — that degrades to "candidate failed, fall through",
// never to accepting forged data — and a compromised broker signs whatever it
// wants, unchanged versus today (SPEC v1 §1).
package client

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openrung/internal/relay"
)

// RelaySignatureHeader carries the broker's detached signature over the exact
// raw response body bytes: "ed25519;<key_id>;<base64_std_signature>" (§2.1).
const RelaySignatureHeader = "X-OpenRung-Relays-Signature"

// notAfterSkewAllowance is how far a device clock may run fast before a fresh,
// validly signed list is rejected as expired: not_after must be at least
// now − 5 min (§5.2, allowance raised from 2 min in §14).
const notAfterSkewAllowance = 5 * time.Minute

// Pinned relay-list signing public keys — raw 32-byte Ed25519 keys as
// lowercase hex, ordered active then standby (§4.2). These are the production
// operator keys; rotating them means promoting the standby on the broker and
// shipping a release with the new ordered set (§11). A CI guard test verifies
// each constant against the committed vector in testdata/signing_vectors.json
// so a truncated or typo'd constant fails CI instead of promotion day.
const (
	relaySigningKeyActiveHex  = "176c03cbc70833285abcea75f2a0e137bd687629142408c22806a86308bd4974"
	relaySigningKeyStandbyHex = "5b2698cfa7a796c671a30aabd5475d55095b91464221f051837eb8fe01f36ea2"
)

// pinnedKey pairs a raw Ed25519 public key with its derived key_id so advisory
// routing (§4.2) does not re-hash on every response.
type pinnedKey struct {
	id  string
	key ed25519.PublicKey
}

// pinnedRelayKeys is the ordered pinned set every non-loopback relay list must
// verify against. Package var (not const) only so tests can substitute a
// throwaway key via PinRelayListKeysForTest — the production private seeds are
// offline and can never sign test fixtures.
var pinnedRelayKeys = mustPinnedKeys(relaySigningKeyActiveHex, relaySigningKeyStandbyHex)

// mustPinnedKeys decodes hex key constants into the ordered pinned set. It
// panics on malformed input: the constants are compile-time operator data, and
// a client that cannot pin its keys must not start.
func mustPinnedKeys(keysHex ...string) []pinnedKey {
	keys := make([]pinnedKey, 0, len(keysHex))
	for _, keyHex := range keysHex {
		raw, err := hex.DecodeString(keyHex)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("pinned relay signing key %q is not 32 hex-encoded bytes", keyHex))
		}
		key := ed25519.PublicKey(raw)
		keys = append(keys, pinnedKey{id: signingKeyID(key), key: key})
	}
	return keys
}

// signingKeyID derives the advisory key identifier: lowercase hex of the first
// 8 bytes of SHA-256 over the raw 32-byte public key (§2.2).
func signingKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// PinRelayListKeysForTest replaces the pinned key set and returns a restore
// func for defer. It exists ONLY so tests (including desktop/discovery's) can
// sign fixtures with a throwaway key; production code must never call it.
func PinRelayListKeysForTest(pubKeysHex ...string) (restore func()) {
	previous := pinnedRelayKeys
	pinnedRelayKeys = mustPinnedKeys(pubKeysHex...)
	return func() { pinnedRelayKeys = previous }
}

// RelayListVerificationError reports a relay-list response that failed the
// §5.2 verification checks. Every message begins with "unsigned/invalid relay
// list" so the failure surfaces as a signature problem rather than a generic
// network error (§5.2, broker-URL overrides); Reason names the individual
// check so failures stay distinguishable in logs.
type RelayListVerificationError struct {
	Reason string
}

func (e *RelayListVerificationError) Error() string {
	return "unsigned/invalid relay list: " + e.Reason
}

func verificationFailure(format string, args ...any) error {
	return &RelayListVerificationError{Reason: fmt.Sprintf(format, args...)}
}

// ReadVerifiedRelayList drains a 2xx relay-list response, verifies the
// signature over the exact raw body bytes, and parses the SAME buffer (§5.1:
// binary read, verify, then decode — never verify re-serialized objects).
//
// endpoint decides the one exemption: loopback hosts skip verification
// entirely, mirroring EnforceSecureBrokerURL's loopback-http allowance, so a
// local dev broker without a signing key keeps working. Every non-loopback
// candidate — including user broker-URL overrides — hard-requires a valid
// signature from the pinned operator keys; the production broker deploys
// signing before any build carrying this check ships (§10.3).
//
// requestedLimit is the caller's raw limit, normalized exactly like
// RelayListURL so the echoed-limit check compares against what was actually
// sent. The key_id that verified is dropped here for now: reporting it in
// telemetry (§8), the last-known-good cache (§5.4), and the fast-clock
// degraded path all ship in a later client-release phase.
func ReadVerifiedRelayList(resp *http.Response, endpoint string, requestedLimit int) (relay.ListResponse, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return relay.ListResponse{}, fmt.Errorf("read relay list: %w", err)
	}

	if endpointIsLoopback(endpoint) {
		var out relay.ListResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return relay.ListResponse{}, fmt.Errorf("decode relay list: %w", err)
		}
		return out, nil
	}

	out, _, err := verifyRelayList(pinnedRelayKeys, relaySignatureHeaderValue(resp.Header), body,
		relay.ChannelAPI, effectiveRelayLimit(requestedLimit), time.Now())
	return out, err
}

// verifyRelayList runs the §5.2 checks: parse the signature header, verify the
// signature over the raw body bytes against the pinned keys, then parse the
// same buffer and check channel, echoed limit, and freshness. It returns the
// parsed response and the key_id of the pinned key that verified (§5.1's
// keyIdUsed, a compromise-detection signal for telemetry).
func verifyRelayList(keys []pinnedKey, sigHeader string, body []byte, expectedChannel string, requestedLimit int, now time.Time) (relay.ListResponse, string, error) {
	headerKeyID, sig, err := parseRelaySignatureHeader(sigHeader)
	if err != nil {
		return relay.ListResponse{}, "", err
	}

	// key_id is advisory routing only (§4.2): try the pinned key it names
	// first, then fall back to every pinned key, so a wrong or unknown key_id
	// costs one wasted verify rather than an outage.
	verifiedKeyID := ""
	for _, candidate := range orderKeysByAdvisoryID(keys, headerKeyID) {
		if ed25519.Verify(candidate.key, body, sig) {
			verifiedKeyID = candidate.id
			break
		}
	}
	if verifiedKeyID == "" {
		return relay.ListResponse{}, "", verificationFailure("signature does not verify under any pinned key (header key_id %q)", headerKeyID)
	}

	// Decode the exact buffer that was just verified.
	var out relay.ListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return relay.ListResponse{}, "", verificationFailure("signed body is not valid JSON: %v", err)
	}
	if out.Channel != expectedChannel {
		return relay.ListResponse{}, "", verificationFailure("channel %q does not match the %q channel this candidate was fetched from", out.Channel, expectedChannel)
	}
	// The limit echo exists only on the API channel; mirrors serve the full
	// list and omit it (§2.2).
	if expectedChannel == relay.ChannelAPI && out.Limit != requestedLimit {
		return relay.ListResponse{}, "", verificationFailure("echoed limit %d does not match requested limit %d", out.Limit, requestedLimit)
	}
	if out.NotAfter.IsZero() {
		return relay.ListResponse{}, "", verificationFailure("signed body carries no not_after freshness bound")
	}
	if out.NotAfter.Before(now.Add(-notAfterSkewAllowance)) {
		return relay.ListResponse{}, "", verificationFailure("list expired: not_after %s is past even with the %s clock-skew allowance (local time %s)",
			out.NotAfter.Format(time.RFC3339), notAfterSkewAllowance, now.Format(time.RFC3339))
	}
	return out, verifiedKeyID, nil
}

// parseRelaySignatureHeader splits "ed25519;<key_id>;<base64_std_signature>"
// (§2.1): exactly three ';'-separated fields, the literal algorithm string,
// and a standard-base64 64-byte signature. The missing-header message is the
// one a user sees when pointing a release build at a broker that has not
// deployed signing, so it says what is wrong and what to do.
func parseRelaySignatureHeader(header string) (keyID string, sig []byte, err error) {
	if header == "" {
		return "", nil, verificationFailure("response carries no %s header — this broker has not enabled relay-list signing (this build requires a signing broker; plain-JSON responses are accepted only from loopback broker URLs for development)", RelaySignatureHeader)
	}
	fields := strings.Split(header, ";")
	if len(fields) != 3 {
		return "", nil, verificationFailure("malformed %s header: want 3 ';'-separated fields, got %d", RelaySignatureHeader, len(fields))
	}
	if fields[0] != "ed25519" {
		return "", nil, verificationFailure("unsupported signature algorithm %q (want ed25519)", fields[0])
	}
	sig, decodeErr := base64.StdEncoding.DecodeString(fields[2])
	if decodeErr != nil {
		return "", nil, verificationFailure("signature is not valid standard base64: %v", decodeErr)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", nil, verificationFailure("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	return fields[1], sig, nil
}

// orderKeysByAdvisoryID puts the pinned key(s) whose key_id matches the
// header's advisory value first, followed by the rest in pinned order (§4.2).
func orderKeysByAdvisoryID(keys []pinnedKey, headerKeyID string) []pinnedKey {
	ordered := make([]pinnedKey, 0, len(keys))
	for _, k := range keys {
		if k.id == headerKeyID {
			ordered = append(ordered, k)
		}
	}
	for _, k := range keys {
		if k.id != headerKeyID {
			ordered = append(ordered, k)
		}
	}
	return ordered
}

// relaySignatureHeaderValue reads the signature header case-insensitively
// (§2.1: HTTP/2 and /3 lowercase header names on the wire). Header.Get already
// canonicalizes its key, which covers every stdlib transport; the fold-scan
// fallback additionally covers hand-built header maps that bypassed
// canonicalization (stub round-trippers in tests).
func relaySignatureHeaderValue(h http.Header) string {
	if v := h.Get(RelaySignatureHeader); v != "" {
		return v
	}
	for name, values := range h {
		if strings.EqualFold(name, RelaySignatureHeader) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// endpointIsLoopback reports whether the request endpoint targets a loopback
// host — the dev-flow signature exemption, matching EnforceSecureBrokerURL's
// loopback-http allowance. An unparseable endpoint is not loopback: fail
// closed and demand a signature.
func endpointIsLoopback(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return hostIsLoopback(parsed.Hostname())
}
