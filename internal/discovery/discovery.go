// Package discovery fetches relay candidates from the broker with staggered
// multi-URL failover (see FirstReachable) and 429/Retry-After awareness.
//
// Shared request, identity, caching, redirect, TLS, signature, and race policy
// lives in brokerapi. This package owns only client relay decoding and the
// compatibility types consumed by the engine's UI and recovery path.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openrung/openrung/brokerapi"

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
	// Platform selects the brokerapi platform-identification header. Empty
	// means PlatformDesktop: the desktop app predates this field and its wire
	// behavior must stay unchanged.
	Platform brokerapi.Platform
	// HTTPClient overrides brokerapi's ECH-capable default client (tests inject
	// a stub).
	HTTPClient *http.Client
	// Stagger overrides the interval at which FirstReachable starts additional
	// candidates (tests shorten it). Zero or negative means
	// brokerapi.DefaultDiscoveryStagger.
	Stagger time.Duration
}

// platform resolves the effective platform header selection for opts.
func (o Options) platform() brokerapi.Platform {
	if o.Platform == "" {
		return brokerapi.PlatformDesktop
	}
	return o.Platform
}

// ListRelays fetches from a single broker endpoint. A 429 returns a
// *RateLimitedError carrying Retry-After; other non-2xx statuses return a
// plain error. brokerapi verifies every non-loopback relay list before exposing
// its exact JSON bytes for decoding into the desktop-owned relay model.
func ListRelays(ctx context.Context, brokerURL string, opts Options) (relay.ListResponse, error) {
	list, err := brokerapi.NewClient(opts.HTTPClient, brokerapi.Options{
		AppVersion: client.AppVersion(),
		Platform:   opts.platform(),
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

// FirstReachable races exact-endpoint-authenticated candidates with a staggered
// start (happy-eyeballs style). The mobile native binding calls this same
// brokerapi implementation rather than carrying a separate race policy.
// candidate[0] starts immediately; every stagger interval
// (brokerapi.DefaultDiscoveryStagger unless Options.Stagger overrides it) with
// no success yet, the next candidate in that trust phase joins the race. The
// first SUCCESS in the phase wins and every other in-flight attempt is canceled.
//
// Endpoint-unbound fronts are held in a separate fallback phase. They do not
// start merely because a stagger elapsed; every exact-endpoint candidate must
// fail first. This keeps an ordinary slow response from downgrading discovery
// to a front that authenticates only its CDN operator. Within either phase,
// priority is expressed through the head start, so a later peer can still win.
// If every candidate fails, the original first candidate's error remains the
// diagnostic. With a single candidate this reduces to exactly one attempt whose
// error propagates unchanged.
//
// When candidates.OverrideFirst is set, URLs[0] is a GENUINE user override
// (see brokerapi.BrokerCandidates) and racing it would betray the user's choice:
// a custom broker that is merely slower than the stagger would silently lose
// to a default front. The override is therefore attempted strictly first,
// alone, with its full per-attempt timeout — no default is contacted while it
// is pending — and it wins on any success, exactly like the old sequential
// loop. Only when the override FAILS does the phased default policy above start
// over the REMAINING candidates. If the override and every remaining candidate
// fail, the override's error is surfaced — it is candidates.URLs[0], so the
// all-fail diagnostic is unchanged — except when the caller's ctx was canceled
// mid-race, which still surfaces the cancellation.
func FirstReachable(ctx context.Context, candidates brokerapi.Candidates, opts Options) (Fetch, error) {
	fetch, err := brokerapi.NewClient(opts.HTTPClient, brokerapi.Options{
		AppVersion: client.AppVersion(),
		Platform:   opts.platform(),
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
