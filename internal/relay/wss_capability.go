package relay

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// WSSCapabilitySpecV1 is intentionally separate from IdentitySpecV1. Adding
// fronts to the deployed identity statement would invalidate existing relay
// implementations during a rolling broker upgrade.
const WSSCapabilitySpecV1 = "openrung-relay-wss-capability-v1"

const (
	// WSSBridgePath is the sole public WebSocket path exposed by every
	// relay-local sidecar. A front URL cannot select any other sidecar route.
	WSSBridgePath = "/api/v1/wss-bridge"

	MaxWSSFronts        = 4
	MaxWSSFrontIDBytes  = 64
	MaxWSSFrontURLBytes = 512
	// Capability and identity expiry are required to match exactly, so keep
	// their signer TTL and verifier ceiling tied together as well.
	MaxWSSCapabilityProofWindow = MaxIdentityProofWindow
	WSSCapabilityProofTTL       = IdentityProofTTLDirect
)

var (
	ErrWSSCapabilityIncomplete = errors.New("wss_fronts, wss_capability_proof, and wss_capability_expires_at must be supplied together")
	ErrWSSCapabilityExpired    = errors.New("relay WSS capability proof expired")
	ErrWSSCapabilityInvalid    = errors.New("invalid relay WSS capability proof")
	ErrWSSFrontInvalid         = errors.New("invalid relay WSS front")
)

type wssCapabilityPayload struct {
	IdentityStatementSHA256 string               `json:"identity_statement_sha256"`
	Fronts                  []WSSFrontDescriptor `json:"fronts"`
}

// NormalizeWSSFronts validates and canonicalizes a relay's complete CDN front
// set. The result is sorted by ID so signatures, registration retries, and
// exact broker echoes are deterministic. Fronts must be ordinary CDN DNS names
// on the standard WSS port; raw IPs would defeat the censorship-resistance
// purpose of the feature, while alternate ports, paths, queries, and userinfo
// would create unnecessary routing ambiguity.
func NormalizeWSSFronts(fronts []WSSFrontDescriptor) ([]WSSFrontDescriptor, error) {
	if len(fronts) == 0 {
		return nil, nil
	}
	if len(fronts) > MaxWSSFronts {
		return nil, fmt.Errorf("%w: at most %d fronts are allowed", ErrWSSFrontInvalid, MaxWSSFronts)
	}

	normalized := make([]WSSFrontDescriptor, 0, len(fronts))
	for index, front := range fronts {
		if strings.ContainsAny(front.ID, "\r\n\t") || strings.ContainsAny(front.URL, "\r\n\t") {
			return nil, fmt.Errorf("%w: front %d contains control whitespace", ErrWSSFrontInvalid, index)
		}
		id := strings.ToLower(strings.TrimSpace(front.ID))
		if !validWSSFrontID(id) {
			return nil, fmt.Errorf("%w: front %d ID must be 1..%d lowercase letters, digits, '.', '_', or '-'", ErrWSSFrontInvalid, index, MaxWSSFrontIDBytes)
		}
		if front.ProtocolVersion != WSSProtocolVersion {
			return nil, fmt.Errorf("%w: front %q protocol_version must be %d", ErrWSSFrontInvalid, id, WSSProtocolVersion)
		}

		rawURL := strings.TrimSpace(front.URL)
		if rawURL == "" || len(rawURL) > MaxWSSFrontURLBytes {
			return nil, fmt.Errorf("%w: front %q URL is empty, oversized, or contains control whitespace", ErrWSSFrontInvalid, id)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Opaque != "" || parsed.Host == "" {
			return nil, fmt.Errorf("%w: front %q URL must be an absolute hierarchical URL", ErrWSSFrontInvalid, id)
		}
		if !strings.EqualFold(parsed.Scheme, "wss") {
			return nil, fmt.Errorf("%w: front %q URL must use wss", ErrWSSFrontInvalid, id)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return nil, fmt.Errorf("%w: front %q URL may not contain userinfo, a query, or a fragment", ErrWSSFrontInvalid, id)
		}
		if parsed.Port() != "" || parsed.Host != parsed.Hostname() {
			return nil, fmt.Errorf("%w: front %q URL must use the default WSS port", ErrWSSFrontInvalid, id)
		}
		host := strings.ToLower(parsed.Hostname())
		if !validWSSFrontDNSName(host) || net.ParseIP(host) != nil {
			return nil, fmt.Errorf("%w: front %q URL host must be a CDN DNS name, not an IP literal", ErrWSSFrontInvalid, id)
		}
		if parsed.Path != WSSBridgePath || parsed.RawPath != "" {
			return nil, fmt.Errorf("%w: front %q URL path must be %s", ErrWSSFrontInvalid, id, WSSBridgePath)
		}

		parsed.Scheme = "wss"
		parsed.Host = host
		normalized = append(normalized, WSSFrontDescriptor{
			ID:              id,
			URL:             parsed.String(),
			ProtocolVersion: WSSProtocolVersion,
		})
	}

	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	for index := range normalized {
		if index > 0 && normalized[index-1].ID == normalized[index].ID {
			return nil, fmt.Errorf("%w: duplicate front ID %q", ErrWSSFrontInvalid, normalized[index].ID)
		}
		for previous := 0; previous < index; previous++ {
			if normalized[previous].URL == normalized[index].URL {
				return nil, fmt.Errorf("%w: duplicate front URL", ErrWSSFrontInvalid)
			}
		}
	}
	return normalized, nil
}

func validWSSFrontID(id string) bool {
	if len(id) == 0 || len(id) > MaxWSSFrontIDBytes {
		return false
	}
	isAlphaNumeric := func(char byte) bool {
		return (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
	}
	if !isAlphaNumeric(id[0]) || !isAlphaNumeric(id[len(id)-1]) {
		return false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '.', '_', '-':
			continue
		default:
			return false
		}
	}
	return true
}

func validWSSFrontDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func validateWSSCapabilityEligibility(req RegisterRequest) error {
	transport := strings.ToLower(strings.TrimSpace(req.Transport))
	if transport == "" {
		transport = TransportDirect
	}
	if transport != TransportDirect || req.NodeClass != NodeClassFoundation || req.ExitMode != ExitModeDirect || req.PublicPort != 443 {
		return fmt.Errorf("%w: WSS requires a direct-transport, direct-exit Foundation relay on public port 443", ErrWSSCapabilityInvalid)
	}
	return nil
}

// WSSCapabilityStatement returns the exact separately-domain-separated bytes
// signed by a relay identity key. It binds both the legacy identity statement
// (without changing that statement's wire format) and the ordered front list.
func WSSCapabilityStatement(req RegisterRequest, expires string) ([]byte, error) {
	fronts, err := NormalizeWSSFronts(req.WSSFronts)
	if err != nil {
		return nil, err
	}
	identityDigest := sha256.Sum256(IdentityStatement(req, req.IdentityExpiresAt))
	payload, err := json.Marshal(wssCapabilityPayload{
		IdentityStatementSHA256: base64.RawStdEncoding.EncodeToString(identityDigest[:]),
		Fronts:                  fronts,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal WSS capability: %w", err)
	}
	statement := WSSCapabilitySpecV1 + "\n" + expires + "\n" + base64.RawStdEncoding.EncodeToString(payload)
	return []byte(statement), nil
}

// SignWSSCapability signs the current registration and ordered front list.
// Callers must first fill the ordinary relay identity fields so the capability
// statement is bound to the exact identity proof being registered.
func SignWSSCapability(priv ed25519.PrivateKey, req RegisterRequest, expiresAt time.Time) (proof, expires string, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", "", errors.New("WSS capability signer requires an Ed25519 private key")
	}
	if err := validateWSSCapabilityEligibility(req); err != nil {
		return "", "", err
	}
	normalized, err := NormalizeWSSFronts(req.WSSFronts)
	if err != nil {
		return "", "", err
	}
	if len(normalized) == 0 {
		return "", "", fmt.Errorf("%w: at least one front is required", ErrWSSCapabilityInvalid)
	}
	if !slices.Equal(normalized, req.WSSFronts) {
		return "", "", fmt.Errorf("%w: fronts must be normalized and sorted before signing", ErrWSSCapabilityInvalid)
	}
	if req.IdentityPublicKey == "" || req.IdentityProof == "" || req.IdentityExpiresAt == "" {
		return "", "", fmt.Errorf("%w: a signed stable identity is required before signing WSS capability", ErrWSSCapabilityInvalid)
	}
	expectedPublicKey := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if req.IdentityPublicKey != expectedPublicKey {
		return "", "", fmt.Errorf("%w: capability signer does not match the relay identity", ErrWSSCapabilityInvalid)
	}
	expires = expiresAt.UTC().Format(time.RFC3339)
	if expires != req.IdentityExpiresAt {
		return "", "", fmt.Errorf("%w: capability and identity expiries must match", ErrWSSCapabilityInvalid)
	}
	statement, err := WSSCapabilityStatement(req, expires)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, statement)), expires, nil
}

// VerifyWSSCapability verifies the separate front advertisement proof and
// returns nil for a registration that does not advertise WSS. It reuses
// VerifyIdentity so no future store caller can accept a capability whose
// identity possession proof is missing or invalid.
func VerifyWSSCapability(req RegisterRequest, now time.Time) error {
	noFronts := len(req.WSSFronts) == 0
	noProof := req.WSSCapabilityProof == "" && req.WSSCapabilityExpiresAt == ""
	if noFronts && noProof {
		return nil
	}
	if noFronts || req.WSSCapabilityProof == "" || req.WSSCapabilityExpiresAt == "" {
		return ErrWSSCapabilityIncomplete
	}
	if len(req.WSSFronts) > MaxWSSFronts {
		return fmt.Errorf("%w: at most %d fronts are allowed", ErrWSSCapabilityInvalid, MaxWSSFronts)
	}
	if err := validateWSSCapabilityEligibility(req); err != nil {
		return err
	}
	normalized, err := NormalizeWSSFronts(req.WSSFronts)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWSSCapabilityInvalid, err)
	}
	if !slices.Equal(normalized, req.WSSFronts) {
		return fmt.Errorf("%w: fronts are not in canonical order", ErrWSSCapabilityInvalid)
	}
	expiresAt, err := time.Parse(time.RFC3339, req.WSSCapabilityExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: wss_capability_expires_at must be RFC 3339", ErrWSSCapabilityInvalid)
	}
	if !expiresAt.After(now) {
		return ErrWSSCapabilityExpired
	}
	if expiresAt.After(now.Add(MaxWSSCapabilityProofWindow)) {
		return fmt.Errorf("%w: expiry is more than %s in the future", ErrWSSCapabilityInvalid, MaxWSSCapabilityProofWindow)
	}
	identityExpiresAt, err := time.Parse(time.RFC3339, req.IdentityExpiresAt)
	if err != nil || !expiresAt.Equal(identityExpiresAt) {
		return fmt.Errorf("%w: capability and identity expiries must match", ErrWSSCapabilityInvalid)
	}
	identityKey, err := VerifyIdentity(req, now)
	if err != nil || identityKey == nil {
		return fmt.Errorf("%w: a valid stable relay identity is required", ErrWSSCapabilityInvalid)
	}
	signature, err := base64.StdEncoding.DecodeString(req.WSSCapabilityProof)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: proof must be a base64 64-byte Ed25519 signature", ErrWSSCapabilityInvalid)
	}
	statement, err := WSSCapabilityStatement(req, req.WSSCapabilityExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWSSCapabilityInvalid, err)
	}
	if !ed25519.Verify(identityKey, statement, signature) {
		return ErrWSSCapabilityInvalid
	}
	return nil
}
