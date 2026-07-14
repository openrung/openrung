package volunteer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"openrung/internal/relay"
)

// BrokerClient speaks the relay side of the broker HTTP API: registration
// and heartbeats. The zero HTTPClient falls back to http.DefaultClient; Token
// is optional (anonymous registration when the broker allows it).
type BrokerClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	// RequireSecureTransport refuses plaintext non-loopback broker URLs and
	// all redirects. It is enabled for the high-value foundation credential;
	// loopback HTTP remains available for local integration tests.
	RequireSecureTransport bool
}

// Register announces the relay and returns the broker-minted descriptor.
func (b BrokerClient) Register(ctx context.Context, req relay.RegisterRequest) (relay.Descriptor, error) {
	var desc relay.Descriptor
	if err := b.postJSON(ctx, "/api/v1/volunteers/register", req, &desc); err != nil {
		return relay.Descriptor{}, err
	}
	return desc, nil
}

// Heartbeat renews the relay's lease. A pruned relay yields an APIError with
// status 404 that IsRelayNotFound recognizes.
func (b BrokerClient) Heartbeat(ctx context.Context, id string) error {
	var resp relay.HeartbeatResponse
	return b.postJSON(ctx, "/api/v1/volunteers/"+id+"/heartbeat", map[string]bool{"ok": true}, &resp)
}

func (b BrokerClient) postJSON(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	requestURL := strings.TrimRight(b.BaseURL, "/") + path
	if b.RequireSecureTransport {
		if err := requireSecureBrokerURL(requestURL); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	httpClient := b.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if b.RequireSecureTransport {
		// Clone the caller's client so the security policy neither mutates a
		// shared client nor depends on its CheckRedirect setting. Broker API
		// endpoints are canonical and should never need a redirect; rejecting
		// every redirect also makes an HTTPS-to-HTTP Authorization leak
		// impossible.
		secureClient := *httpClient
		secureClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("secure broker request refused redirect to %s", req.URL.Redacted())
		}
		httpClient = &secureClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr relay.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		return &APIError{Path: path, StatusCode: resp.StatusCode, Message: apiErr.Error, RetryAfter: resp.Header.Get("Retry-After")}
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}

	return nil
}

func requireSecureBrokerURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse broker URL %q: %w", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackBrokerHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("secure broker requests require an https URL (got %q); plaintext http is allowed only on loopback", rawURL)
}

func isLoopbackBrokerHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// APIError is a non-2xx broker response.
type APIError struct {
	Path       string
	StatusCode int
	Message    string
	// RetryAfter is the raw Retry-After header (seconds), present on 429s so
	// callers can honour the broker's backoff request.
	RetryAfter string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("broker %s: %s", e.Path, e.Message)
}

// IsRelayNotFound reports whether err is the broker telling us the relay lease
// expired and the ID is gone — the caller should re-register.
func IsRelayNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Message == "relay not found"
}
