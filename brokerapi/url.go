// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// EnforceSecureBrokerURL accepts HTTPS everywhere and plain HTTP only for
// loopback development. Broker traffic can carry persistent identity before a
// tunnel exists, so there is no off-device cleartext exception.
func EnforceSecureBrokerURL(baseURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return nil, fmt.Errorf("broker URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse broker URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("broker URL must include scheme and host")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("broker URL must not contain user information")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return parsed, nil
	case "http":
		if hostIsLoopback(parsed.Hostname()) {
			return parsed, nil
		}
		return nil, fmt.Errorf("refusing cleartext broker URL %q: use https (plain http is allowed only to localhost)", trimmed)
	default:
		return nil, fmt.Errorf("broker URL scheme must be https, got %q", parsed.Scheme)
	}
}

func hostIsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func endpointIsLoopback(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && hostIsLoopback(parsed.Hostname())
}

func endpointURL(baseURL, endpointPath string) (string, error) {
	parsed, err := EnforceSecureBrokerURL(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.Trim(parsed.Path, "/")
	endpointPath = strings.Trim(endpointPath, "/")
	if basePath == "" {
		parsed.Path = "/" + endpointPath
	} else {
		parsed.Path = "/" + basePath + "/" + endpointPath
	}
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func effectiveRelayLimit(limit int) int {
	if limit < 1 {
		return DefaultRelayLimit
	}
	return limit
}

// RelayListURL resolves GET /api/v1/relays and normalizes a non-positive limit
// to DefaultRelayLimit.
func RelayListURL(baseURL string, limit int) (string, error) {
	endpoint, err := endpointURL(baseURL, "api/v1/relays")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(effectiveRelayLimit(limit)))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// TelemetryURL resolves POST /api/v1/telemetry/events.
func TelemetryURL(baseURL string) (string, error) {
	return endpointURL(baseURL, "api/v1/telemetry/events")
}

// WSSTicketURL resolves POST /api/v1/wss/tickets.
func WSSTicketURL(baseURL string) (string, error) {
	return endpointURL(baseURL, "api/v1/wss/tickets")
}

// HealthURL resolves GET /healthz.
func HealthURL(baseURL string) (string, error) {
	return endpointURL(baseURL, "healthz")
}

// SpeedTestURL resolves GET /api/v1/speed-test for the requested byte count.
func SpeedTestURL(baseURL string, byteCount int) (string, error) {
	if byteCount < 1 || byteCount > MaxSpeedTestBytes {
		return "", fmt.Errorf("speed-test byte count must be between 1 and %d", MaxSpeedTestBytes)
	}
	endpoint, err := endpointURL(baseURL, "api/v1/speed-test")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("bytes", strconv.Itoa(byteCount))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
