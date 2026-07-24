// Package discovery fetches relay candidates from the broker with staggered
// multi-URL failover (see FirstReachable) and 429/Retry-After awareness.
//
// Shared request, identity, caching, redirect, TLS, signature, and race policy
// lives in brokerapi. This package owns only desktop relay decoding and the
// compatibility types consumed by the UI and recovery path.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/desktop/config"
	"openrung/internal/client"
	"openrung/internal/relay"
)

// Fetch is a successful relay fetch together with the endpoint that served it,
// so the caller can pin later requests (telemetry, connect) to the same broker.
type Fetch struct {
	BrokerURL string
	Response  relay.ListResponse
}

// RateLimitedError remains as a compatibility alias for desktop recovery code.
type RateLimitedError = brokerapi.RateLimitedError

// Options identify the caller to the broker. When ClientID and SessionID are
// both set they are sent as identity headers so the broker records a
// client_seen telemetry event for the request.
type Options struct {
	Limit     int
	ClientID  string
	SessionID string
	// HTTPClient overrides brokerapi's ECH-capable default client (tests inject
	// a stub).
	HTTPClient *http.Client
	// Stagger overrides the interval at which FirstReachable starts additional
	// candidates (tests shorten it). Zero or negative means
	// config.DiscoveryStagger.
	Stagger time.Duration
}

// ListRelays fetches from a single broker endpoint. A 429 returns a
// *RateLimitedError carrying Retry-After; other non-2xx statuses return a
// plain error. brokerapi verifies every non-loopback relay list before exposing
// its exact JSON bytes for decoding into the desktop-owned relay model.
func ListRelays(ctx context.Context, brokerURL string, opts Options) (relay.ListResponse, error) {
	list, err := brokerapi.NewClient(opts.HTTPClient, brokerapi.Options{
		AppVersion: client.AppVersion(),
		Platform:   brokerapi.PlatformDesktop,
	}).ListRelays(ctx, brokerURL, brokerapi.ListOptions{
		Limit: opts.Limit,
		Identity: brokerapi.Identity{
			ClientID:  opts.ClientID,
			SessionID: opts.SessionID,
		},
	})
	if err != nil {
		return relay.ListResponse{}, err
	}
	return decodeRelayList(list.JSON())
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
//
// When candidates.OverrideFirst is set, URLs[0] is a GENUINE user override
// (see config.BrokerCandidates) and racing it would betray the user's choice:
// a custom broker that is merely slower than the stagger would silently lose
// to a default front. The override is therefore attempted strictly first,
// alone, with its full per-attempt timeout — no default is contacted while it
// is pending — and it wins on any success, exactly like the old sequential
// loop. Only when the override FAILS does the staggered race above start over
// the REMAINING candidates (the first of them immediately, the next one
// stagger later, and so on). If the override and every remaining candidate
// fail, the override's error is surfaced — it is candidates.URLs[0], so the
// all-fail diagnostic is unchanged — except when the caller's ctx was
// cancelled mid-race, which still surfaces the cancellation.
func FirstReachable(ctx context.Context, candidates config.Candidates, opts Options) (Fetch, error) {
	fetch, err := brokerapi.NewClient(opts.HTTPClient, brokerapi.Options{
		AppVersion: client.AppVersion(),
		Platform:   brokerapi.PlatformDesktop,
	}).FirstReachable(ctx, candidates, brokerapi.ListOptions{
		Limit: opts.Limit,
		Identity: brokerapi.Identity{
			ClientID:  opts.ClientID,
			SessionID: opts.SessionID,
		},
		Stagger: opts.Stagger,
	})
	if err != nil {
		return Fetch{}, err
	}
	response, err := decodeRelayList(fetch.RelayList.JSON())
	if err != nil {
		return Fetch{}, err
	}
	return Fetch{BrokerURL: fetch.BrokerURL, Response: response}, nil
}

func decodeRelayList(body []byte) (relay.ListResponse, error) {
	var response relay.ListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return relay.ListResponse{}, fmt.Errorf("decode verified relay list: %w", err)
	}
	return response, nil
}
