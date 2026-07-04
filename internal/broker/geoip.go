package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"openrung/internal/relay"
)

// DefaultGeoIPEndpoint matches the provider the Android client and CLI already
// use for their own public-IP geo (see internal/clienttelemetry). The looked-up
// host is appended to the endpoint path.
const DefaultGeoIPEndpoint = "https://ipwho.is/"

const (
	geoIPLookupTimeout    = 4 * time.Second
	geoIPSuccessCacheTTL  = 24 * time.Hour
	geoIPFailureCacheTTL  = 10 * time.Minute
	geoIPMaxCacheEntries  = 4096
	geoIPMaxResponseBytes = 64 << 10
)

// GeoIPResolver resolves the physical location of a relay's public endpoint.
// Lookups are best-effort: the broker registers and heartbeats relays normally
// when a lookup fails.
type GeoIPResolver interface {
	Lookup(ctx context.Context, host string) (relay.GeoLocation, error)
}

// HTTPGeoIPResolver queries an ip-geolocation HTTP endpoint (ipwho.is by
// default; ip-api.com-style responses are understood too) and caches results
// per host so registrations and heartbeat backfills do not hammer the
// provider's rate limits.
type HTTPGeoIPResolver struct {
	endpoint string
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]geoCacheEntry
}

type geoCacheEntry struct {
	geo       relay.GeoLocation
	err       error
	expiresAt time.Time
}

func NewHTTPGeoIPResolver(endpoint string) *HTTPGeoIPResolver {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = DefaultGeoIPEndpoint
	}
	if !strings.HasSuffix(endpoint, "/") {
		endpoint += "/"
	}
	return &HTTPGeoIPResolver{
		endpoint: endpoint,
		client:   &http.Client{Timeout: geoIPLookupTimeout},
		now:      time.Now,
		cache:    make(map[string]geoCacheEntry),
	}
}

func (r *HTTPGeoIPResolver) Lookup(ctx context.Context, host string) (relay.GeoLocation, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return relay.GeoLocation{}, errors.New("geoip lookup requires a host")
	}

	if entry, ok := r.cached(host); ok {
		return entry.geo, entry.err
	}

	geo, err := r.fetch(ctx, host)
	// Context cancellations describe the caller, not the host; retrying later
	// may well succeed, so don't poison the cache with them.
	if err == nil || !errors.Is(err, context.Canceled) {
		r.store(host, geo, err)
	}
	return geo, err
}

func (r *HTTPGeoIPResolver) cached(host string) (geoCacheEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[host]
	if !ok || r.now().After(entry.expiresAt) {
		return geoCacheEntry{}, false
	}
	return entry, true
}

func (r *HTTPGeoIPResolver) store(host string, geo relay.GeoLocation, err error) {
	ttl := geoIPSuccessCacheTTL
	if err != nil {
		ttl = geoIPFailureCacheTTL
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if len(r.cache) >= geoIPMaxCacheEntries {
		for key, entry := range r.cache {
			if now.After(entry.expiresAt) {
				delete(r.cache, key)
			}
		}
		if len(r.cache) >= geoIPMaxCacheEntries {
			r.cache = make(map[string]geoCacheEntry)
		}
	}
	r.cache[host] = geoCacheEntry{geo: geo, err: err, expiresAt: now.Add(ttl)}
}

func (r *HTTPGeoIPResolver) fetch(ctx context.Context, host string) (relay.GeoLocation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint+url.PathEscape(host), nil)
	if err != nil {
		return relay.GeoLocation{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return relay.GeoLocation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return relay.GeoLocation{}, fmt.Errorf("geoip endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, geoIPMaxResponseBytes))
	if err != nil {
		return relay.GeoLocation{}, err
	}
	return decodeGeoLocation(body)
}

// geoIPLookupResponse covers both ipwho.is ("success"/"country_code"/
// "latitude") and ip-api.com ("status"/"countryCode"/"lat") response shapes.
type geoIPLookupResponse struct {
	Success          *bool   `json:"success"`
	Status           string  `json:"status"`
	Message          string  `json:"message"`
	City             string  `json:"city"`
	Country          string  `json:"country"`
	CountryCode      string  `json:"country_code"`
	CountryCodeCamel string  `json:"countryCode"`
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
	Lat              float64 `json:"lat"`
	Lon              float64 `json:"lon"`
}

func decodeGeoLocation(body []byte) (relay.GeoLocation, error) {
	var parsed geoIPLookupResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return relay.GeoLocation{}, fmt.Errorf("decode geoip response: %w", err)
	}
	failed := (parsed.Success != nil && !*parsed.Success) ||
		(parsed.Status != "" && parsed.Status != "success")
	if failed {
		message := strings.TrimSpace(parsed.Message)
		if message == "" {
			message = "lookup failed"
		}
		return relay.GeoLocation{}, fmt.Errorf("geoip endpoint rejected lookup: %s", message)
	}

	geo := relay.GeoLocation{
		City:        strings.TrimSpace(parsed.City),
		Country:     strings.TrimSpace(parsed.Country),
		CountryCode: strings.TrimSpace(firstNonEmpty(parsed.CountryCode, parsed.CountryCodeCamel)),
		Latitude:    parsed.Latitude,
		Longitude:   parsed.Longitude,
	}
	if geo.Latitude == 0 && geo.Longitude == 0 {
		geo.Latitude, geo.Longitude = parsed.Lat, parsed.Lon
	}
	if geo == (relay.GeoLocation{}) {
		return relay.GeoLocation{}, errors.New("geoip endpoint returned no location fields")
	}
	return geo, nil
}
