// Package wssbridge implements broker ticket policy and relay-local origin,
// replay, source-limit, and fixed-target policy around wsscore. The CDN and
// relay-local sidecar never receive Reality key material and cannot decrypt
// the inner stream.
package wssbridge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openrung/openrung/wsscore"
)

const (
	TicketVersion  = 1
	TicketAudience = "openrung-wss-bridge"

	MaxTicketBytes     = wsscore.MaxTicketBytes
	MaxTicketLifetime  = 5 * time.Minute
	MaxTicketClockSkew = 2 * time.Minute
	MaxTicketStreams   = 1024

	ticketPrefix        = "v1"
	ticketSignatureSpec = "openrung-wss-bridge-ticket-v1\n"
)

var (
	ErrInvalidTicket = errors.New("invalid WSS bridge ticket")
	ErrExpiredTicket = errors.New("expired WSS bridge ticket")
)

// Claims is the complete and deliberately narrow authority in a ticket. A
// ticket identifies one relay and one advertised CDN front; it carries no dial
// address. The sidecar's only target comes from local configuration.
type Claims struct {
	Version    int    `json:"v"`
	Audience   string `json:"aud"`
	JTI        string `json:"jti"`
	RelayID    string `json:"relay_id"`
	FrontID    string `json:"front_id"`
	IssuedAt   int64  `json:"iat"`
	NotBefore  int64  `json:"nbf"`
	ExpiresAt  int64  `json:"exp"`
	MaxStreams int    `json:"max_streams"`
}

func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// TicketOptions can only tighten the protocol ceilings.
type TicketOptions struct {
	Now         func() time.Time
	MaxLifetime time.Duration
	ClockSkew   time.Duration
}

func (o TicketOptions) normalized() (TicketOptions, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxLifetime == 0 {
		o.MaxLifetime = MaxTicketLifetime
	}
	if o.MaxLifetime < time.Second || o.MaxLifetime > MaxTicketLifetime {
		return TicketOptions{}, fmt.Errorf("ticket max lifetime must be within [1s, %s]", MaxTicketLifetime)
	}
	if o.ClockSkew < 0 || o.ClockSkew > MaxTicketClockSkew {
		return TicketOptions{}, fmt.Errorf("ticket clock skew must be within [0, %s]", MaxTicketClockSkew)
	}
	return o, nil
}

type TicketSigner struct {
	key   ed25519.PrivateKey
	keyID string
	opts  TicketOptions
}

func NewTicketSigner(key ed25519.PrivateKey, opts TicketOptions) (*TicketSigner, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("ticket signing key must be %d bytes", ed25519.PrivateKeySize)
	}
	normalized, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	keyCopy := append(ed25519.PrivateKey(nil), key...)
	return &TicketSigner{
		key: keyCopy, keyID: TicketKeyID(key.Public().(ed25519.PublicKey)), opts: normalized,
	}, nil
}

func (s *TicketSigner) KeyID() string { return s.keyID }

func (s *TicketSigner) Sign(claims Claims) (string, error) {
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return "", errors.New("ticket signer is not initialized")
	}
	if err := validateClaims(claims, s.opts.Now().UTC(), s.opts, false, ""); err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal ticket claims: %w", err)
	}
	signature := ed25519.Sign(s.key, ticketStatement(payload))
	token := strings.Join([]string{
		ticketPrefix,
		s.keyID,
		base64.RawURLEncoding.EncodeToString(payload),
		base64.RawURLEncoding.EncodeToString(signature),
	}, ".")
	if len(token) > MaxTicketBytes {
		return "", fmt.Errorf("ticket exceeds %d-byte maximum", MaxTicketBytes)
	}
	return token, nil
}

// TicketVerifier accepts an overlapping set of Ed25519 keys for rotation and
// binds every accepted ticket to one exact locally configured relay ID.
type TicketVerifier struct {
	keys         map[string]ed25519.PublicKey
	localRelayID string
	opts         TicketOptions
}

func NewTicketVerifier(keys map[string]ed25519.PublicKey, localRelayID string, opts TicketOptions) (*TicketVerifier, error) {
	if !validID(localRelayID, 1, 128) {
		return nil, errors.New("local relay ID is invalid")
	}
	if len(keys) == 0 {
		return nil, errors.New("at least one ticket verification key is required")
	}
	normalized, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	keyCopy := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("ticket verification key %q must be %d bytes", id, ed25519.PublicKeySize)
		}
		derived := TicketKeyID(key)
		if id != derived {
			return nil, fmt.Errorf("ticket verification key ID %q does not match derived ID %q", id, derived)
		}
		keyCopy[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &TicketVerifier{keys: keyCopy, localRelayID: localRelayID, opts: normalized}, nil
}

func (v *TicketVerifier) LocalRelayID() string {
	if v == nil {
		return ""
	}
	return v.localRelayID
}

// Verify authenticates raw canonical claim bytes before decoding or using
// them, then enforces relay binding and freshness.
func (v *TicketVerifier) Verify(token string) (Claims, error) {
	if v == nil || len(v.keys) == 0 {
		return Claims{}, fmt.Errorf("%w: verifier is not initialized", ErrInvalidTicket)
	}
	if len(token) == 0 || len(token) > MaxTicketBytes {
		return Claims{}, fmt.Errorf("%w: ticket size is outside bounds", ErrInvalidTicket)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != ticketPrefix {
		return Claims{}, fmt.Errorf("%w: malformed compact encoding", ErrInvalidTicket)
	}
	key, ok := v.keys[parts[1]]
	if !ok {
		return Claims{}, fmt.Errorf("%w: unknown signing key", ErrInvalidTicket)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(payload) == 0 || len(payload) > MaxTicketBytes || base64.RawURLEncoding.EncodeToString(payload) != parts[2] {
		return Claims{}, fmt.Errorf("%w: malformed payload", ErrInvalidTicket)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[3] {
		return Claims{}, fmt.Errorf("%w: malformed signature", ErrInvalidTicket)
	}
	if !ed25519.Verify(key, ticketStatement(payload), signature) {
		return Claims{}, fmt.Errorf("%w: signature verification failed", ErrInvalidTicket)
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var claims Claims
	if err := decoder.Decode(&claims); err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not valid claims JSON", ErrInvalidTicket)
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(canonical, payload) {
		return Claims{}, fmt.Errorf("%w: claims encoding is not canonical", ErrInvalidTicket)
	}
	if err := validateClaims(claims, v.opts.Now().UTC(), v.opts, true, v.localRelayID); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func ticketStatement(payload []byte) []byte {
	statement := make([]byte, 0, len(ticketSignatureSpec)+len(payload))
	statement = append(statement, ticketSignatureSpec...)
	return append(statement, payload...)
}

func TicketKeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// ParseTicketPublicKeys parses comma-separated standard-base64 public keys,
// optionally prefixed with their derived 16-hex-character key ID.
func ParseTicketPublicKeys(value string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, rawEntry := range strings.Split(value, ",") {
		entry := strings.TrimSpace(rawEntry)
		if entry == "" {
			continue
		}
		id, encoded := "", entry
		if separator := strings.IndexByte(entry, '='); separator == 16 && isLowerHex(entry[:separator]) {
			id, encoded = entry[:separator], entry[separator+1:]
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("ticket public key must be standard-base64 encoding of %d bytes", ed25519.PublicKeySize)
		}
		key := ed25519.PublicKey(raw)
		derived := TicketKeyID(key)
		if id != "" && id != derived {
			return nil, fmt.Errorf("ticket public key ID %q does not match derived ID %q", id, derived)
		}
		if _, duplicate := keys[derived]; duplicate {
			return nil, fmt.Errorf("duplicate ticket public key %q", derived)
		}
		keys[derived] = append(ed25519.PublicKey(nil), key...)
	}
	if len(keys) == 0 {
		return nil, errors.New("no ticket public keys configured")
	}
	return keys, nil
}

func validateClaims(c Claims, now time.Time, opts TicketOptions, verifying bool, localRelayID string) error {
	fail := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidTicket, message) }
	if c.Version != TicketVersion {
		return fail("unsupported version")
	}
	if c.Audience != TicketAudience {
		return fail("wrong audience")
	}
	if !validID(c.JTI, 16, 128) {
		return fail("invalid JTI")
	}
	if !validID(c.RelayID, 1, 128) || (verifying && c.RelayID != localRelayID) {
		return fail("wrong relay")
	}
	if wsscore.ValidateFrontID(c.FrontID) != nil {
		return fail("invalid front")
	}
	if c.MaxStreams < 1 || c.MaxStreams > MaxTicketStreams {
		return fail("max_streams outside bounds")
	}
	if c.IssuedAt <= 0 || c.NotBefore <= 0 || c.ExpiresAt <= 0 || c.ExpiresAt <= c.IssuedAt || c.NotBefore < c.IssuedAt || c.NotBefore >= c.ExpiresAt {
		return fail("invalid time ordering")
	}
	lifetimeSeconds := c.ExpiresAt - c.IssuedAt
	if lifetimeSeconds > int64(opts.MaxLifetime/time.Second) || lifetimeSeconds > int64(MaxTicketLifetime/time.Second) {
		return fail("lifetime exceeds maximum")
	}
	if time.Unix(c.IssuedAt, 0).After(now.Add(opts.ClockSkew)) {
		return fail("issued in the future")
	}
	if !verifying {
		if !time.Unix(c.ExpiresAt, 0).After(now) {
			return fail("claims are not currently signable")
		}
		return nil
	}
	if time.Unix(c.NotBefore, 0).After(now.Add(opts.ClockSkew)) {
		return fail("not yet valid")
	}
	if !time.Unix(c.ExpiresAt, 0).After(now.Add(-opts.ClockSkew)) {
		return ErrExpiredTicket
	}
	return nil
}

func validID(value string, minLen, maxLen int) bool {
	if len(value) < minLen || len(value) > maxLen {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '-', '_', '.', ':':
			continue
		default:
			return false
		}
	}
	return true
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
