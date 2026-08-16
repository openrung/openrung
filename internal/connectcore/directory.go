package connectcore

import (
	"context"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/discovery"
	"openrung/internal/relay"
)

// relayFetcher fetches a relay list from the broker. It abstracts
// discovery.FirstReachable so the cache is unit-testable without a live
// broker. brokerURL narrows the front race to one override endpoint; empty
// means the default fronts.
type relayFetcher func(ctx context.Context, brokerURL string, opts discovery.Options) (relay.ListResponse, error)

// directoryCache serves the exit-node map's relay list with a hard floor on
// broker request rate. The map auto-refreshes, so without this a chatty or
// buggy frontend could trip the broker's per-IP 429 limit (broker PR #5); the
// cache caps outbound requests at one per MinDirectoryRefreshInterval
// and hands back the last good list in between.
type directoryCache struct {
	fetcher relayFetcher
	// now is injectable so tests need not sleep. Nil means time.Now.
	now func() time.Time

	// flightMu serializes broker fetches (single-flight): concurrent refreshes
	// wait for the one in flight and are then served from the cache it filled,
	// so simultaneous requests cannot multiply broker load past the rate floor.
	flightMu sync.Mutex

	mu        sync.Mutex
	cached    *relay.ListResponse
	cachedURL string // broker override that produced the snapshot ("" = default fronts)
	fetchedAt time.Time
}

// Signed directory responses allow five minutes of clock skew during
// verification. The cache applies the same finite allowance, then refuses to
// serve the snapshot even when the broker is unreachable: not_after is a
// replay bound, not merely a hint to refresh when convenient.
const directoryNotAfterSkewAllowance = 5 * time.Minute

func newDirectoryCache() *directoryCache {
	return &directoryCache{
		fetcher: func(ctx context.Context, brokerURL string, opts discovery.Options) (relay.ListResponse, error) {
			fetch, err := discovery.FirstReachable(ctx, brokerapi.BrokerCandidates(brokerURL), opts)
			if err != nil {
				return relay.ListResponse{}, err
			}
			return fetch.Response, nil
		},
		now: time.Now,
	}
}

func (d *directoryCache) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// ListRelaysForDirectory returns the broker's relay list for the host UI to
// aggregate into map regions (the TS loadExitNodeDirectory, ported from
// mobile, does the grouping). Running the fetch in the engine reuses the
// failover/429 logic, attaches identity headers, and avoids a webview
// cross-origin request to the broker.
func (s *Engine) ListRelaysForDirectory() (relay.ListResponse, error) {
	return s.directory.fetch(context.Background(), "", s.identityForDirectory())
}

// DirectoryRelay is one usable directory entry plus what the ranker measured
// for it: TCP connect latency in milliseconds, nil when the relay sat past the
// probed head or its probe failed.
type DirectoryRelay struct {
	Relay   relay.Descriptor
	ProbeMS *int64
}

// RankedDirectory returns the directory's usable relays in client-ranked order
// — the same latency-bucket ranking the connect ladder walks (see ranker.go) —
// for host UIs that list candidates. brokerURL narrows the fetch to one
// override endpoint, empty for the default fronts. Each call re-probes up to
// RelayRankMaxProbes relays, so hosts should call it on a user-visible refresh,
// not on a timer; the list fetch itself stays rate-limited by the cache.
func (s *Engine) RankedDirectory(ctx context.Context, brokerURL string) ([]DirectoryRelay, error) {
	resp, err := s.directory.fetch(ctx, brokerURL, s.identityForDirectory())
	if err != nil {
		return nil, err
	}
	ranked := rankByTCPLatency(
		ctx,
		usableRelays(resp),
		RelayRankMaxProbes,
		RelayRankProbeTimeout,
		RelayRankBucketMS,
		s.relayDialer(),
	)
	out := make([]DirectoryRelay, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, DirectoryRelay{Relay: r.relay, ProbeMS: r.probeMS})
	}
	return out, nil
}

// identityForDirectory reads the current identity without blocking on the
// connect lock. sessionID is empty until a session begins (phase 2+), in which
// case discovery omits the identity headers.
func (s *Engine) identityForDirectory() discovery.Options {
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	id, err := clientID()
	if err != nil {
		id = ""
	}
	return discovery.Options{
		Limit:     DirectoryRelayLimit,
		ClientID:  id,
		SessionID: sessionID,
	}
}

func (d *directoryCache) fetch(ctx context.Context, brokerURL string, opts discovery.Options) (relay.ListResponse, error) {
	d.flightMu.Lock()
	defer d.flightMu.Unlock()

	if cached, ok := d.snapshotFor(brokerURL, true); ok {
		return cached, nil
	}

	response, err := d.fetcher(ctx, brokerURL, opts)
	if err != nil {
		// Serve the last good list on a transient broker failure (rate-limit,
		// blocked edge) so the map does not empty out mid-session — but never a
		// list a different broker served: a stale cross-broker snapshot would
		// point targeted connects at relays the requested broker never listed.
		if cached, ok := d.snapshotFor(brokerURL, false); ok {
			return cached, nil
		}
		return relay.ListResponse{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.cached = &response
	d.cachedURL = brokerURL
	d.fetchedAt = d.clock()
	return response, nil
}

// snapshotFor returns the cached list when it was served for the same broker
// override and is still within its not_after bound; withinInterval additionally
// requires it to be inside the refresh-rate floor (the cache-hit condition,
// versus the serve-stale-on-error fallback).
func (d *directoryCache) snapshotFor(brokerURL string, withinInterval bool) (relay.ListResponse, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.cachedURL != brokerURL || !directorySnapshotFresh(d.cached, now) {
		return relay.ListResponse{}, false
	}
	if withinInterval && now.Sub(d.fetchedAt) >= MinDirectoryRefreshInterval {
		return relay.ListResponse{}, false
	}
	return *d.cached, true
}

func directorySnapshotFresh(response *relay.ListResponse, now time.Time) bool {
	return response != nil &&
		!response.NotAfter.IsZero() &&
		!response.NotAfter.Before(now.Add(-directoryNotAfterSkewAllowance))
}
