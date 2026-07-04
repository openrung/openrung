package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestHTTPGeoIPResolverParsesIpwhoisAndCaches(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/203.0.113.10" {
			t.Errorf("unexpected lookup path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"203.0.113.10","success":true,"country":"Japan","country_code":"JP","city":"Tokyo","latitude":35.6895,"longitude":139.6917}`))
	}))
	defer server.Close()

	resolver := NewHTTPGeoIPResolver(server.URL)
	want := relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP", Latitude: 35.6895, Longitude: 139.6917}
	for attempt := 0; attempt < 2; attempt++ {
		geo, err := resolver.Lookup(context.Background(), "203.0.113.10")
		if err != nil {
			t.Fatalf("lookup attempt %d: %v", attempt, err)
		}
		if geo != want {
			t.Fatalf("lookup attempt %d = %+v, want %+v", attempt, geo, want)
		}
	}
	if requests != 1 {
		t.Fatalf("expected second lookup to hit the cache, got %d requests", requests)
	}
}

func TestHTTPGeoIPResolverParsesIPAPIShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","country":"Germany","countryCode":"DE","city":"Berlin","lat":52.52,"lon":13.405}`))
	}))
	defer server.Close()

	geo, err := NewHTTPGeoIPResolver(server.URL).Lookup(context.Background(), "198.51.100.7")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	want := relay.GeoLocation{City: "Berlin", Country: "Germany", CountryCode: "DE", Latitude: 52.52, Longitude: 13.405}
	if geo != want {
		t.Fatalf("lookup = %+v, want %+v", geo, want)
	}
}

func TestHTTPGeoIPResolverCachesFailuresUntilTTL(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"reserved range"}`))
	}))
	defer server.Close()

	resolver := NewHTTPGeoIPResolver(server.URL)
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	resolver.now = func() time.Time { return now }

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := resolver.Lookup(context.Background(), "192.168.1.1"); err == nil {
			t.Fatalf("expected rejected lookup to fail on attempt %d", attempt)
		}
	}
	if requests != 1 {
		t.Fatalf("expected failure to be cached, got %d requests", requests)
	}

	now = now.Add(geoIPFailureCacheTTL + time.Second)
	if _, err := resolver.Lookup(context.Background(), "192.168.1.1"); err == nil {
		t.Fatal("expected retried lookup to fail")
	}
	if requests != 2 {
		t.Fatalf("expected failure cache to expire, got %d requests", requests)
	}
}

func TestHTTPGeoIPResolverRejectsEmptyLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	if _, err := NewHTTPGeoIPResolver(server.URL).Lookup(context.Background(), "203.0.113.10"); err == nil {
		t.Fatal("expected lookup without location fields to fail")
	}
}
