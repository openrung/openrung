// Package discovery fetches relay candidates from the broker with multi-URL
// failover and 429/Retry-After awareness.
//
// It reuses internal/client's URL builder and the relay wire types, but issues
// the HTTP request itself so it can read the status code and Retry-After header
// — internal/client.BrokerClient.ListRelays collapses every non-2xx into an
// opaque error, which is enough for the CLI but not for the GUI, whose map
// auto-refreshes and must therefore back off politely when the broker starts
// returning 429 (added in broker PR #5).
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"openrung/internal/client"
	"openrung/internal/relay"
)

// requestTimeout bounds a single broker request, matching the mobile app's
// 15s AbortController deadline (src/net/brokerClient.ts).
const requestTimeout = 15 * time.Second

// Fetch is a successful relay fetch together with the endpoint that served it,
// so the caller can pin later requests (telemetry, connect) to the same broker.
type Fetch struct {
	BrokerURL string
	Response  relay.ListResponse
}

// RateLimitedError reports a broker 429. RetryAfter is the parsed Retry-After
// value, or 0 when the header was absent or unparseable.
type RateLimitedError struct {
	BrokerURL  string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("broker %s rate-limited (retry after %s)", e.BrokerURL, e.RetryAfter)
	}
	return fmt.Sprintf("broker %s rate-limited", e.BrokerURL)
}

// Options identify the caller to the broker. When ClientID and SessionID are
// both set they are sent as identity headers so the broker records a
// client_seen telemetry event for the request.
type Options struct {
	Limit     int
	ClientID  string
	SessionID string
	// HTTPClient overrides the default client (tests inject a stub). When nil a
	// client with requestTimeout is used.
	HTTPClient *http.Client
}

// ListRelays fetches from a single broker endpoint. A 429 returns a
// *RateLimitedError carrying Retry-After; other non-2xx statuses return a
// plain error.
func ListRelays(ctx context.Context, brokerURL string, opts Options) (relay.ListResponse, error) {
	endpoint, err := client.RelayListURL(brokerURL, opts.Limit)
	if err != nil {
		return relay.ListResponse{}, err
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return relay.ListResponse{}, err
	}
	req.Header.Set("X-OpenRung-App-Version", client.AppVersion())
	// Mark the platform the way the mobile app marks itself with X-OpenRung-RN,
	// so the broker can distinguish desktop clients in telemetry.
	req.Header.Set("X-OpenRung-Desktop", desktopPlatform())
	// The broker edge serves the relay list with a long max-age; without this
	// the platform HTTP cache replays a stale list and new relays never appear.
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")
	if opts.ClientID != "" && opts.SessionID != "" {
		req.Header.Set("X-OpenRung-Client-ID", opts.ClientID)
		req.Header.Set("X-OpenRung-Session-ID", opts.SessionID)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return relay.ListResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return relay.ListResponse{}, &RateLimitedError{
			BrokerURL:  brokerURL,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return relay.ListResponse{}, brokerStatusError(resp)
	}

	var out relay.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return relay.ListResponse{}, fmt.Errorf("decode relay list: %w", err)
	}
	return out, nil
}

// FirstReachable tries each candidate in order and returns the first success
// with the endpoint that served it, mirroring the mobile app's firstReachable.
// A blocked or rate-limited primary therefore never takes discovery offline as
// long as one candidate answers. If every candidate fails, the last error is
// returned (so a caller can surface a Retry-After when the whole list is
// rate-limited).
func FirstReachable(ctx context.Context, candidates []string, opts Options) (Fetch, error) {
	if len(candidates) == 0 {
		return Fetch{}, errors.New("no broker endpoints configured")
	}
	var lastErr error
	for _, brokerURL := range candidates {
		response, err := ListRelays(ctx, brokerURL, opts)
		if err != nil {
			lastErr = err
			continue
		}
		return Fetch{BrokerURL: brokerURL, Response: response}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no broker endpoints reachable")
	}
	return Fetch{}, lastErr
}

// parseRetryAfter handles both Retry-After forms: delta-seconds (RFC 9110) and
// an HTTP-date. Unparseable or absent values yield 0.
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func brokerStatusError(resp *http.Response) error {
	var apiErr relay.ErrorResponse
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(body, &apiErr)
	if apiErr.Error == "" {
		apiErr.Error = resp.Status
	}
	return fmt.Errorf("broker list relays: %s", apiErr.Error)
}
