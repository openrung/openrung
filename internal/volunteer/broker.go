package volunteer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"openrung/internal/relay"
)

const (
	canonicalRegisterPath      = "/api/v1/relays/register"
	legacyRegisterPath         = "/api/v1/volunteers/register"
	canonicalHeartbeatPathBase = "/api/v1/relays/"
	legacyHeartbeatPathBase    = "/api/v1/volunteers/"
	maxBrokerErrorBodyBytes    = 64 << 10
	legacyServeMuxNotFoundBody = "404 page not found\n"
)

// BrokerClient speaks the relay side of the broker HTTP API: registration
// and heartbeats. The zero HTTPClient falls back to http.DefaultClient; Token
// is optional (anonymous registration when the broker allows it). A
// BrokerClient must not be copied after first use so its route selection stays
// shared.
type BrokerClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	// RequireSecureTransport refuses plaintext non-loopback broker URLs. It is
	// enabled for the high-value foundation credential; loopback HTTP remains
	// available for local integration tests. All broker requests refuse
	// redirects regardless of this setting.
	RequireSecureTransport bool

	// legacyRoutes is broker-wide for this client: once either canonical write
	// endpoint proves unavailable, registration and heartbeats both stay on the
	// compatibility route family. Atomic state makes concurrent desktop-engine
	// requests race-safe and keeps the transition monotonic.
	legacyRoutes atomic.Bool
}

// Register announces the relay and returns the broker-minted descriptor.
func (b *BrokerClient) Register(ctx context.Context, req relay.RegisterRequest) (relay.Descriptor, error) {
	if b.legacyRoutes.Load() {
		return b.registerAt(ctx, legacyRegisterPath, req)
	}

	desc, err := b.registerAt(ctx, canonicalRegisterPath, req)
	if !isRouteUnsupported(err) {
		return desc, err
	}

	b.legacyRoutes.Store(true)
	return b.registerAt(ctx, legacyRegisterPath, req)
}

func (b *BrokerClient) registerAt(ctx context.Context, path string, req relay.RegisterRequest) (relay.Descriptor, error) {
	var desc relay.Descriptor
	if err := b.postJSON(ctx, path, req, &desc); err != nil {
		return relay.Descriptor{}, err
	}
	return desc, nil
}

// Heartbeat renews the relay's lease. A pruned relay yields an APIError with
// status 404 that IsRelayNotFound recognizes.
func (b *BrokerClient) Heartbeat(ctx context.Context, id string) error {
	if b.legacyRoutes.Load() {
		return b.heartbeatAt(ctx, legacyHeartbeatPathBase, id)
	}

	err := b.heartbeatAt(ctx, canonicalHeartbeatPathBase, id)
	if !isRouteUnsupported(err) {
		return err
	}

	b.legacyRoutes.Store(true)
	return b.heartbeatAt(ctx, legacyHeartbeatPathBase, id)
}

func (b *BrokerClient) heartbeatAt(ctx context.Context, pathBase, id string) error {
	var resp relay.HeartbeatResponse
	return b.postJSON(ctx, pathBase+id+"/heartbeat", map[string]bool{"ok": true}, &resp)
}

func (b *BrokerClient) postJSON(ctx context.Context, path string, body any, out any) error {
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
	// Clone the caller's client so the policy neither mutates a shared client
	// nor depends on its CheckRedirect setting. Broker write endpoints never
	// need a redirect; refusing every redirect prevents forwarding the bearer
	// token and ensures a redirected 404/405 cannot trigger legacy fallback.
	noRedirectClient := *httpClient
	noRedirectClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("broker request refused redirect to %s", req.URL.Redacted())
	}
	httpClient = &noRedirectClient
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBrokerErrorBodyBytes+1))
		bodyWithinLimit := len(errorBody) <= maxBrokerErrorBodyBytes
		var apiErr relay.ErrorResponse
		if readErr == nil && bodyWithinLimit {
			_ = json.Unmarshal(errorBody, &apiErr)
		}
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
		legacyMux404 := resp.StatusCode == http.StatusNotFound &&
			readErr == nil && bodyWithinLimit &&
			strings.EqualFold(strings.TrimSpace(mediaType), "text/plain") &&
			bytes.Equal(errorBody, []byte(legacyServeMuxNotFoundBody))
		return &APIError{
			Path:             path,
			StatusCode:       resp.StatusCode,
			Message:          apiErr.Error,
			RetryAfter:       resp.Header.Get("Retry-After"),
			routeUnsupported: resp.StatusCode == http.StatusMethodNotAllowed || legacyMux404,
		}
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

	routeUnsupported bool
}

func isRouteUnsupported(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.routeUnsupported
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
