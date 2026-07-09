package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"openrung/internal/relay"
)

const defaultRelayLimit = 5

type BrokerClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// ListRelays fetches relay candidates from the broker. When clientID and
// sessionID are non-empty they are sent as identity headers so the broker can
// auto-record a client_seen telemetry event for the request.
func (c BrokerClient) ListRelays(ctx context.Context, limit int, clientID, sessionID string) (relay.ListResponse, error) {
	endpoint, err := RelayListURL(c.BaseURL, limit)
	if err != nil {
		return relay.ListResponse{}, err
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return relay.ListResponse{}, err
	}
	if clientID != "" && sessionID != "" {
		req.Header.Set("X-OpenRung-Client-ID", clientID)
		req.Header.Set("X-OpenRung-Session-ID", sessionID)
		req.Header.Set("X-OpenRung-App-Version", AppVersion())
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return relay.ListResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return relay.ListResponse{}, brokerStatusError(resp)
	}

	var out relay.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return relay.ListResponse{}, fmt.Errorf("decode relay list: %w", err)
	}
	return out, nil
}

func RelayListURL(baseURL string, limit int) (string, error) {
	if limit < 1 {
		limit = defaultRelayLimit
	}

	parsed, err := EnforceSecureBrokerURL(baseURL)
	if err != nil {
		return "", err
	}

	basePath := strings.Trim(parsed.Path, "/")
	pathParts := []string{"api/v1/relays"}
	if basePath != "" {
		pathParts = append([]string{basePath}, pathParts...)
	}
	parsed.Path = "/" + strings.Join(pathParts, "/")

	query := parsed.Query()
	query.Set("limit", fmt.Sprintf("%d", limit))
	parsed.RawQuery = query.Encode()

	return parsed.String(), nil
}

// EnforceSecureBrokerURL parses baseURL and rejects cleartext broker endpoints.
// https is allowed to any host; plain http is allowed ONLY to a loopback host
// (local development). Plaintext http to any other host is refused so an on-path
// network observer cannot read or tamper with the pre-tunnel broker traffic —
// the relay directory seeds the entire VPN config and both the directory and
// telemetry requests carry the persistent client identity headers. It returns
// the parsed URL so callers can build endpoint paths on it.
//
// (A pinned bare-IP HTTPS fallback for a blocked edge can be layered on later;
// until then there is no cleartext path off the device.)
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

// hostIsLoopback reports whether host is localhost or a loopback IP literal.
func hostIsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// BrokerStatusError reports a broker non-2xx response and carries the status
// code so error classification can label it (429 → rate_limited, otherwise
// http_<code>) without matching on the message string. The discovery fetch path
// reuses this type for the same reason.
type BrokerStatusError struct {
	StatusCode int
	Message    string
}

func (e *BrokerStatusError) Error() string {
	return fmt.Sprintf("broker list relays: %s", e.Message)
}

// HTTPStatus exposes the broker response status for error classification.
func (e *BrokerStatusError) HTTPStatus() int { return e.StatusCode }

func brokerStatusError(resp *http.Response) error {
	var apiErr relay.ErrorResponse
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(body, &apiErr)
	if apiErr.Error == "" {
		apiErr.Error = strings.TrimSpace(string(body))
	}
	if apiErr.Error == "" {
		apiErr.Error = resp.Status
	}
	return &BrokerStatusError{StatusCode: resp.StatusCode, Message: apiErr.Error}
}
