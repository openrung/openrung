package wsscore

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

var ErrInvalidFront = errors.New("invalid WSS front")

// NormalizeFronts validates, canonicalizes, and sorts a complete advertised
// front set. IDs and URLs are unique after normalization.
func NormalizeFronts(fronts []Front) ([]Front, error) {
	if len(fronts) == 0 {
		return nil, nil
	}
	if len(fronts) > MaxFronts {
		return nil, fmt.Errorf("%w: at most %d fronts are allowed", ErrInvalidFront, MaxFronts)
	}
	normalized := make([]Front, 0, len(fronts))
	for index, front := range fronts {
		id, err := NormalizeFrontID(front.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: front %d ID: %v", ErrInvalidFront, index, err)
		}
		if front.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("%w: front %q protocol_version must be %d", ErrInvalidFront, id, ProtocolVersion)
		}
		frontURL, err := NormalizeFrontURL(front.URL)
		if err != nil {
			return nil, fmt.Errorf("%w: front %q URL: %v", ErrInvalidFront, id, err)
		}
		normalized = append(normalized, Front{ID: id, URL: frontURL, ProtocolVersion: ProtocolVersion})
	}

	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	seenURLs := make(map[string]struct{}, len(normalized))
	for index, front := range normalized {
		if index > 0 && normalized[index-1].ID == front.ID {
			return nil, fmt.Errorf("%w: duplicate front ID %q", ErrInvalidFront, front.ID)
		}
		if _, duplicate := seenURLs[front.URL]; duplicate {
			return nil, fmt.Errorf("%w: duplicate front URL", ErrInvalidFront)
		}
		seenURLs[front.URL] = struct{}{}
	}
	return normalized, nil
}

// NormalizeFrontID canonicalizes an operator-provided front ID.
func NormalizeFrontID(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\t") {
		return "", fmt.Errorf("%w: ID contains control whitespace", ErrInvalidFront)
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	if !validFrontID(normalized) {
		return "", fmt.Errorf("%w: ID must be 1..%d lowercase letters, digits, '.', '_', or '-'", ErrInvalidFront, MaxFrontIDBytes)
	}
	return normalized, nil
}

// ValidateFrontID requires an already-canonical front ID.
func ValidateFrontID(value string) error {
	normalized, err := NormalizeFrontID(value)
	if err != nil {
		return err
	}
	if normalized != value {
		return fmt.Errorf("%w: ID is not canonical", ErrInvalidFront)
	}
	return nil
}

// NormalizeFrontURL canonicalizes an operator-provided production front URL.
// It accepts only WSS CDN DNS names on the default port and the fixed bridge
// path. Raw IPs, alternate ports, credentials, queries, fragments, and escaped
// paths are rejected.
func NormalizeFrontURL(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", fmt.Errorf("%w: URL contains control whitespace", ErrInvalidFront)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxFrontURLBytes {
		return "", fmt.Errorf("%w: URL is empty or oversized", ErrInvalidFront)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("%w: URL must be an absolute hierarchical URL", ErrInvalidFront)
	}
	if !strings.EqualFold(parsed.Scheme, "wss") {
		return "", fmt.Errorf("%w: URL must use wss", ErrInvalidFront)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: URL may not contain userinfo, a query, or a fragment", ErrInvalidFront)
	}
	if parsed.Port() != "" || parsed.Host != parsed.Hostname() {
		return "", fmt.Errorf("%w: URL must use the default WSS port", ErrInvalidFront)
	}
	host := strings.ToLower(parsed.Hostname())
	if !validFrontDNSName(host) || net.ParseIP(host) != nil {
		return "", fmt.Errorf("%w: URL host must be a CDN DNS name, not an IP literal", ErrInvalidFront)
	}
	if parsed.Path != BridgePath || parsed.RawPath != "" {
		return "", fmt.Errorf("%w: URL path must be %s", ErrInvalidFront, BridgePath)
	}
	parsed.Scheme = "wss"
	parsed.Host = host
	return parsed.String(), nil
}

// ValidateFrontURL requires a production front URL to be both valid and
// already canonical. DialClient uses this stricter form so it never silently
// changes a URL covered by a relay signature and ticket response.
func ValidateFrontURL(raw string) error {
	normalized, err := NormalizeFrontURL(raw)
	if err != nil {
		return err
	}
	if normalized != raw {
		return fmt.Errorf("%w: URL is not canonical", ErrInvalidFront)
	}
	return nil
}

func validFrontID(value string) bool {
	if len(value) == 0 || len(value) > MaxFrontIDBytes {
		return false
	}
	isAlphaNumeric := func(char byte) bool {
		return (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
	}
	if !isAlphaNumeric(value[0]) || !isAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for i := range len(value) {
		char := value[i]
		if isAlphaNumeric(char) || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func validFrontDNSName(host string) bool {
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
		for i := range len(label) {
			char := label[i]
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	// A public CDN name has an alphabetic top-level label. Requiring its first
	// character to be a letter also rejects legacy, non-canonical IPv4
	// spellings (for example 127.1, 0177.0.0.1, and 127.0.0.0x1) that some
	// resolvers interpret as an address even though net.ParseIP does not.
	finalLabel := labels[len(labels)-1]
	if finalLabel[0] < 'a' || finalLabel[0] > 'z' {
		return false
	}
	return true
}
