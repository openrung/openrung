// Package discovery fetches relay candidates from the broker with staggered
// multi-URL failover (see FirstReachable) and 429/Retry-After awareness.
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

	"openrung/desktop/config"
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

// HTTPStatus reports the 429 status so error classification labels a
// rate-limited fetch as rate_limited without importing this concrete type.
func (e *RateLimitedError) HTTPStatus() int { return http.StatusTooManyRequests }

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
	// Stagger overrides the interval at which FirstReachable starts additional
	// candidates (tests shorten it). Zero or negative means
	// config.DiscoveryStagger.
	Stagger time.Duration
}

// ListRelays fetches from a single broker endpoint. A 429 returns a
// *RateLimitedError carrying Retry-After; other non-2xx statuses return a
// plain error. Successful responses pass through client.ReadVerifiedRelayList
// — the shared relay-list signature check — so the GUI and the CLI verify
// identically: any non-loopback broker must sign the list with a pinned
// operator key or the candidate fails and the race falls through.
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

	return client.ReadVerifiedRelayList(resp, endpoint, opts.Limit)
}

// FirstReachable races the candidates with a staggered start (happy-eyeballs
// style), mirroring the mobile app's firstReachable. candidate[0] starts
// immediately; every stagger interval (config.DiscoveryStagger unless
// Options.Stagger overrides it) with no success yet, the next candidate joins
// the race. The first SUCCESS wins: its fetch is returned with the endpoint
// that served it — so the caller can pin later requests to the same broker —
// and every other in-flight attempt is aborted via context cancellation.
// Priority is expressed only through the head start: a later candidate that
// answers first wins even while an earlier one is still pending. A blocked or
// rate-limited primary therefore never takes discovery offline as long as one
// candidate answers, and a hung primary costs one stagger interval instead of
// a full request timeout. If EVERY candidate fails, the FIRST candidate's
// error is returned — the primary's failure is the meaningful diagnostic (and
// carries a Retry-After when the primary was rate-limited). With a single
// candidate this reduces to exactly one attempt whose error propagates
// unchanged.
func FirstReachable(ctx context.Context, candidates []string, opts Options) (Fetch, error) {
	if len(candidates) == 0 {
		return Fetch{}, errors.New("no broker endpoints configured")
	}

	stagger := opts.Stagger
	if stagger <= 0 {
		stagger = config.DiscoveryStagger
	}

	// raceCtx aborts every in-flight loser the moment a winner returns (or the
	// caller gives up); each attempt's HTTP request is bound to it.
	raceCtx, cancelRace := context.WithCancel(ctx)
	defer cancelRace()

	type attemptResult struct {
		index int
		fetch Fetch
		err   error
	}
	// Buffered so a late loser never blocks sending after the winner returned.
	results := make(chan attemptResult, len(candidates))
	start := func(index int) {
		brokerURL := candidates[index]
		go func() {
			response, err := ListRelays(raceCtx, brokerURL, opts)
			if err != nil {
				results <- attemptResult{index: index, err: err}
				return
			}
			results <- attemptResult{index: index, fetch: Fetch{BrokerURL: brokerURL, Response: response}}
		}()
	}

	start(0)
	started := 1
	var tick <-chan time.Time
	if len(candidates) > 1 {
		ticker := time.NewTicker(stagger)
		defer ticker.Stop()
		tick = ticker.C
	}

	failed := 0
	errs := make([]error, len(candidates))
	for {
		select {
		case <-tick:
			start(started)
			started++
			if started == len(candidates) {
				tick = nil
			}
		case res := <-results:
			if res.err == nil {
				cancelRace() // first success wins: abort the losers' requests
				return res.fetch, nil
			}
			errs[res.index] = res.err
			failed++
			if failed == len(candidates) {
				return Fetch{}, errs[0]
			}
		case <-ctx.Done():
			// The caller gave up. raceCtx is a child of ctx, so every in-flight
			// attempt aborts promptly; drain them and surface the primary's error
			// (candidate[0] always started), matching what the attempt itself
			// observed. Candidates that never started are skipped — they could
			// only fail on the dead context.
			for pending := started - failed; pending > 0; pending-- {
				res := <-results
				if res.err == nil {
					// The response completed before the cancellation landed; a
					// success still wins.
					return res.fetch, nil
				}
				errs[res.index] = res.err
			}
			return Fetch{}, errs[0]
		}
	}
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
	return &client.BrokerStatusError{StatusCode: resp.StatusCode, Message: apiErr.Error}
}
