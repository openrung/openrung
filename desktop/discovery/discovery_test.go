package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const relayBody = `{"count":1,"server_time":"2026-07-06T00:00:00Z","relays":[{"id":"r1","public_host":"1.2.3.4","public_port":443}]}`

func TestListRelaysSuccess(t *testing.T) {
	var gotVersion, gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-OpenRung-App-Version")
		gotPlatform = r.Header.Get("X-OpenRung-Desktop")
		w.Write([]byte(relayBody))
	}))
	defer srv.Close()

	resp, err := ListRelays(context.Background(), srv.URL, Options{Limit: 20})
	if err != nil {
		t.Fatalf("ListRelays: %v", err)
	}
	if len(resp.Relays) != 1 || resp.Relays[0].ID != "r1" {
		t.Fatalf("unexpected relays: %+v", resp.Relays)
	}
	if gotVersion == "" {
		t.Error("X-OpenRung-App-Version header not sent")
	}
	if gotPlatform == "" {
		t.Error("X-OpenRung-Desktop header not sent")
	}
}

func TestListRelaysRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := ListRelays(context.Background(), srv.URL, Options{})
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitedError, got %v", err)
	}
	if rl.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", rl.RetryAfter)
	}
}

func TestFirstReachableFailsOverToSecond(t *testing.T) {
	// First candidate 429s, second serves relays. Discovery must fall through
	// so a rate-limited primary never takes the map offline.
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer good.Close()

	fetch, err := FirstReachable(context.Background(), []string{limited.URL, good.URL}, Options{})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != good.URL {
		t.Fatalf("served by %q, want %q", fetch.BrokerURL, good.URL)
	}
	if len(fetch.Response.Relays) != 1 {
		t.Fatalf("unexpected relays: %+v", fetch.Response.Relays)
	}
}

func TestFirstReachableAllFailReturnsLastError(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()

	_, err := FirstReachable(context.Background(), []string{limited.URL, limited.URL}, Options{})
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitedError from exhausted candidates, got %v", err)
	}
}

func TestFirstReachableNoCandidates(t *testing.T) {
	_, err := FirstReachable(context.Background(), nil, Options{})
	if err == nil {
		t.Fatal("want error for empty candidate list")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Errorf("delta-seconds: got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("negative: got %v", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
}
