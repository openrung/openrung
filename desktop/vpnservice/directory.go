package vpnservice

import (
	"context"
	"sync"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/internal/relay"
)

// relayFetcher fetches a relay list from the broker. It abstracts
// discovery.FirstReachable so the cache is unit-testable without a live broker.
type relayFetcher func(ctx context.Context, opts discovery.Options) (relay.ListResponse, error)

// directoryCache serves the exit-node map's relay list with a hard floor on
// broker request rate. The map auto-refreshes, so without this a chatty or
// buggy frontend could trip the broker's per-IP 429 limit (broker PR #5); the
// cache caps outbound requests at one per config.MinDirectoryRefreshInterval
// and hands back the last good list in between.
type directoryCache struct {
	fetcher relayFetcher
	// now is injectable so tests need not sleep. Nil means time.Now.
	now func() time.Time

	mu        sync.Mutex
	cached    *relay.ListResponse
	fetchedAt time.Time
}

func newDirectoryCache() *directoryCache {
	return &directoryCache{
		fetcher: func(ctx context.Context, opts discovery.Options) (relay.ListResponse, error) {
			fetch, err := discovery.FirstReachable(ctx, config.BrokerCandidates(""), opts)
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

// ListRelaysForDirectory is Wails-bound. It returns the broker's relay list for
// the frontend to aggregate into map regions (the TS loadExitNodeDirectory,
// ported from mobile, does the grouping). Running the fetch in Go reuses the
// failover/429 logic, attaches identity headers, and avoids a webview
// cross-origin request to the broker.
func (s *Service) ListRelaysForDirectory() (relay.ListResponse, error) {
	return s.directory.fetch(context.Background(), s.identityForDirectory())
}

// identityForDirectory reads the current identity without blocking on the
// connect lock. sessionID is empty until a session begins (phase 2+), in which
// case discovery omits the identity headers.
func (s *Service) identityForDirectory() discovery.Options {
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	id, err := clientID()
	if err != nil {
		id = ""
	}
	return discovery.Options{
		Limit:     config.DirectoryRelayLimit,
		ClientID:  id,
		SessionID: sessionID,
	}
}

func (d *directoryCache) fetch(ctx context.Context, opts discovery.Options) (relay.ListResponse, error) {
	d.mu.Lock()
	if d.cached != nil && d.clock().Sub(d.fetchedAt) < config.MinDirectoryRefreshInterval {
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
		if d.cached != nil {
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
