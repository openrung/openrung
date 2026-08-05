// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxRelayListBytes = 4 << 20

// ListRelays fetches and verifies one relay-list response. Redirects are
// returned, never followed, so identity cannot cross origins or escape a
// loopback cleartext exception.
func (c *Client) ListRelays(ctx context.Context, brokerURL string, options ListOptions) (RelayList, error) {
	endpoint, err := RelayListURL(brokerURL, options.Limit)
	if err != nil {
		return RelayList{}, err
	}
	ctx, cancel := withRequestTimeout(ctx, DefaultRelayListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RelayList{}, err
	}
	req.Header.Set("Accept", "application/json")
	c.addCommonHeaders(req, options.Identity)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RelayList{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return RelayList{}, &RateLimitedError{
			BrokerURL:  brokerURL,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return RelayList{}, statusError(resp, "list relays", brokerURL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayListBytes+1))
	if err != nil {
		return RelayList{}, fmt.Errorf("read relay list: %w", err)
	}
	if len(body) > maxRelayListBytes {
		return RelayList{}, fmt.Errorf("relay list exceeds %d bytes", maxRelayListBytes)
	}
	return verifyRelayList(
		productionRelayKeys,
		endpoint,
		options.Limit,
		relaySignatureHeaderValue(resp.Header),
		body,
		time.Now(),
	)
}

func statusError(resp *http.Response, operation, brokerURL string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apiError struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &apiError)
	message := strings.TrimSpace(apiError.Error)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}
	return &BrokerStatusError{
		Operation:  operation,
		BrokerURL:  brokerURL,
		StatusCode: resp.StatusCode,
		Message:    message,
	}
}

// FirstReachable races endpoint-bound candidates with a stagger and returns the
// first verified success. Endpoint-unbound fronts are raced in a second phase
// only after every endpoint-bound candidate has failed. A genuine custom
// override is attempted alone first, regardless of its authentication class.
func (c *Client) FirstReachable(ctx context.Context, candidates Candidates, options ListOptions) (Fetch, error) {
	if len(candidates.URLs) == 0 {
		return Fetch{}, errors.New("no broker endpoints configured")
	}

	if candidates.OverrideFirst {
		list, overrideErr := c.ListRelays(ctx, candidates.URLs[0], options)
		if overrideErr == nil {
			return Fetch{BrokerURL: candidates.URLs[0], RelayList: list}, nil
		}
		if ctx.Err() != nil {
			return Fetch{}, ctx.Err()
		}
		if len(candidates.URLs) == 1 {
			return Fetch{}, overrideErr
		}
		fetch, raceErr := c.race(ctx, candidates.URLs[1:], options)
		if raceErr == nil {
			return fetch, nil
		}
		if ctx.Err() != nil {
			return Fetch{}, raceErr
		}
		return Fetch{}, overrideErr
	}
	return c.race(ctx, candidates.URLs, options)
}

func (c *Client) race(ctx context.Context, urls []string, options ListOptions) (Fetch, error) {
	if len(urls) == 0 {
		return Fetch{}, errors.New("no broker endpoints configured")
	}

	endpointBound := make([]string, 0, len(urls))
	endpointUnbound := make([]string, 0, len(urls))
	for _, brokerURL := range urls {
		if EndpointUnboundBrokerFront(brokerURL) {
			endpointUnbound = append(endpointUnbound, brokerURL)
		} else {
			endpointBound = append(endpointBound, brokerURL)
		}
	}

	if len(endpointBound) == 0 {
		return c.racePhase(ctx, endpointUnbound, options)
	}
	if len(endpointUnbound) == 0 {
		return c.racePhase(ctx, endpointBound, options)
	}

	fetch, boundErr := c.racePhase(ctx, endpointBound, options)
	if boundErr == nil {
		return fetch, nil
	}
	if err := ctx.Err(); err != nil {
		return Fetch{}, err
	}

	fetch, unboundErr := c.racePhase(ctx, endpointUnbound, options)
	if unboundErr == nil {
		return fetch, nil
	}
	if err := ctx.Err(); err != nil {
		return Fetch{}, err
	}

	// The old single-phase race returned the error from urls[0] after every
	// candidate failed. Classification can change execution order, but not the
	// caller-visible error choice.
	if EndpointUnboundBrokerFront(urls[0]) {
		return Fetch{}, unboundErr
	}
	return Fetch{}, boundErr
}

func (c *Client) racePhase(ctx context.Context, urls []string, options ListOptions) (Fetch, error) {
	if err := ctx.Err(); err != nil {
		return Fetch{}, err
	}

	stagger := options.Stagger
	if stagger <= 0 {
		stagger = DefaultDiscoveryStagger
	}

	raceCtx, cancelRace := context.WithCancel(ctx)
	defer cancelRace()

	type attemptResult struct {
		index int
		fetch Fetch
		err   error
	}
	results := make(chan attemptResult, len(urls))
	start := func(index int) {
		brokerURL := urls[index]
		go func() {
			list, err := c.ListRelays(raceCtx, brokerURL, options)
			if err != nil {
				results <- attemptResult{index: index, err: err}
				return
			}
			results <- attemptResult{
				index: index,
				fetch: Fetch{BrokerURL: brokerURL, RelayList: list},
			}
		}()
	}

	start(0)
	started := 1
	var tick <-chan time.Time
	if len(urls) > 1 {
		ticker := time.NewTicker(stagger)
		defer ticker.Stop()
		tick = ticker.C
	}

	failed := 0
	errs := make([]error, len(urls))
	for {
		select {
		case <-tick:
			start(started)
			started++
			if started == len(urls) {
				tick = nil
			}
		case result := <-results:
			if result.err == nil {
				cancelRace()
				return result.fetch, nil
			}
			errs[result.index] = result.err
			failed++
			if failed == len(urls) {
				return Fetch{}, errs[0]
			}
		case <-ctx.Done():
			// Return promptly even if a custom RoundTripper is slow to observe
			// cancellation. results is buffered for every attempt, so canceled
			// workers can still finish without blocking after the caller leaves.
			return Fetch{}, ctx.Err()
		}
	}
}
