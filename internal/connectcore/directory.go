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
// discovery.FirstReachable so the cache is unit-testable without a live broker.
type relayFetcher func(ctx context.Context, opts discovery.Options) (relay.ListResponse, error)

// directoryCache serves the exit-node map's relay list with a hard floor on
// broker request rate. The map auto-refreshes, so without this a chatty or
// buggy frontend could trip the broker's per-IP 429 limit (broker PR #5); the
// cache caps outbound requests at one per MinDirectoryRefreshInterval
// and hands back the last good list in between.
type directoryCache struct {
	fetcher relayFetcher
	// now is injectable so tests need not sleep. Nil means time.Now.
	now func() time.Time

	mu        sync.Mutex
	cached    *relay.ListResponse
	fetchedAt time.Time
}

// Signed directory responses allow five minutes of clock skew during
// verification. The cache applies the same finite allowance, then refuses to
// serve the snapshot even when the broker is unreachable: not_after is a
// replay bound, not merely a hint to refresh when convenient.
const directoryNotAfterSkewAllowance = 5 * time.Minute

func newDirectoryCache() *directoryCache {
	return &directoryCache{
		fetcher: func(ctx context.Context, opts discovery.Options) (relay.ListResponse, error) {
			fetch, err := discovery.FirstReachable(ctx, brokerapi.BrokerCandidates(""), opts)
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
	return s.directory.fetch(context.Background(), s.identityForDirectory())
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

func (d *directoryCache) fetch(ctx context.Context, opts discovery.Options) (relay.ListResponse, error) {
	d.mu.Lock()
	now := d.clock()
	if directorySnapshotFresh(d.cached, now) && now.Sub(d.fetchedAt) < MinDirectoryRefreshInterval {
		cached := *d.cached
		d.mu.Unlock()
		return cached, nil
	}
	d.mu.Unlock()

	response, err := d.fetcher(ctx, opts)
	if err != nil {
		// Serve the last good list on a transient broker failure (rate-limit,
		// blocked edge) so the map does not empty out mid-session.
		d.mu.Lock()
		defer d.mu.Unlock()
		if directorySnapshotFresh(d.cached, d.clock()) {
			return *d.cached, nil
		}
		return relay.ListResponse{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.cached = &response
	d.fetchedAt = d.clock()
	return response, nil
}

func directorySnapshotFresh(response *relay.ListResponse, now time.Time) bool {
	return response != nil &&
		!response.NotAfter.IsZero() &&
		!response.NotAfter.Before(now.Add(-directoryNotAfterSkewAllowance))
}
