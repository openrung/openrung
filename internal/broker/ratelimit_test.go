package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPRateLimiterEnforcesBurstThenRefills(t *testing.T) {
	current := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	limiter := newIPRateLimiter(1, 2, 10)
	limiter.now = func() time.Time { return current }

	if !limiter.allow("203.0.113.1") || !limiter.allow("203.0.113.1") {
		t.Fatal("expected burst of 2 to be allowed")
	}
	if limiter.allow("203.0.113.1") {
		t.Fatal("expected third immediate request to be denied")
	}

	current = current.Add(1500 * time.Millisecond)
	if !limiter.allow("203.0.113.1") {
		t.Fatal("expected refill to allow a request after 1.5s at 1/s")
	}
	if limiter.allow("203.0.113.1") {
		t.Fatal("expected partial refill to deny a second request")
	}
}

func TestIPRateLimiterIsolatesSources(t *testing.T) {
	current := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	limiter := newIPRateLimiter(1, 1, 10)
	limiter.now = func() time.Time { return current }

	if !limiter.allow("203.0.113.1") {
		t.Fatal("first source should be allowed")
	}
	if limiter.allow("203.0.113.1") {
		t.Fatal("first source should be exhausted")
	}
	if !limiter.allow("203.0.113.2") {
		t.Fatal("second source must have its own bucket")
	}
}

func TestIPRateLimiterFailsOpenWhenTableFull(t *testing.T) {
	current := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	limiter := newIPRateLimiter(1, 1, 2)
	limiter.now = func() time.Time { return current }

	limiter.allow("203.0.113.1")
	limiter.allow("203.0.113.2")

	if !limiter.allow("203.0.113.3") || !limiter.allow("203.0.113.3") {
		t.Fatal("untracked sources must fail open when the table is full")
	}
	if limiter.allow("203.0.113.1") {
		t.Fatal("tracked sources must stay limited while the table is full")
	}
}

func TestIPRateLimiterSweepsIdleBuckets(t *testing.T) {
	current := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	limiter := newIPRateLimiter(1, 1, 1)
	limiter.now = func() time.Time { return current }

	if !limiter.allow("203.0.113.1") {
		t.Fatal("first source should be allowed")
	}

	// After the idle source has fully refilled it is safe to forget, freeing
	// its slot for a new source, which then gets tracked (not fail-open).
	current = current.Add(2 * time.Second)
	if !limiter.allow("203.0.113.2") {
		t.Fatal("new source should claim the swept slot")
	}
	if limiter.allow("203.0.113.2") {
		t.Fatal("new source must be tracked and limited, not failed open")
	}
}

func TestWSSTicketRateKeySeparatesClientsBehindSharedSource(t *testing.T) {
	resolver := newClientIPResolver(nil)
	key := wssTicketRateKey(resolver)
	request := func(clientID string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/wss/tickets", nil)
		r.RemoteAddr = "203.0.113.9:44321"
		if clientID != "" {
			r.Header.Set("X-OpenRung-Client-ID", clientID)
		}
		return r
	}
	first := key(request("client-1"))
	second := key(request("client-2"))
	if first == second || first == "203.0.113.9" || second == "203.0.113.9" {
		t.Fatalf("valid client keys were not separated and pseudonymized: %q / %q", first, second)
	}
	if got := key(request("")); got != "203.0.113.9" {
		t.Fatalf("missing client ID key = %q, want source fallback", got)
	}
	for _, invalid := range []string{" client", "client\nother", string(make([]byte, 129))} {
		if got := key(request(invalid)); got != "203.0.113.9" {
			t.Fatalf("invalid client ID key = %q, want source fallback", got)
		}
	}
}

func TestWSSTicketRateLimiterDoesNotCollapseSharedSourceClients(t *testing.T) {
	limiter := newIPRateLimiter(1, 1, 10)
	resolver := newClientIPResolver(nil)
	hits := 0
	handler := rateLimitedBy(limiter, wssTicketRateKey(resolver), 10, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	})
	request := func(clientID string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/wss/tickets", nil)
		r.RemoteAddr = "203.0.113.9:44321"
		r.Header.Set("X-OpenRung-Client-ID", clientID)
		return r
	}
	for _, test := range []struct {
		clientID string
		want     int
	}{
		{clientID: "client-1", want: http.StatusNoContent},
		{clientID: "client-1", want: http.StatusTooManyRequests},
		{clientID: "client-2", want: http.StatusNoContent},
	} {
		recorder := httptest.NewRecorder()
		handler(recorder, request(test.clientID))
		if recorder.Code != test.want {
			t.Fatalf("client %q status = %d, want %d", test.clientID, recorder.Code, test.want)
		}
	}
	if hits != 2 {
		t.Fatalf("handler hits = %d, want one request for each client", hits)
	}
}

func TestServerRateLimitsSpeedTest(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	for index := 0; index < speedTestBurst; index++ {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=10", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d inside burst: expected 200, got %d", index+1, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=10", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 past the burst, got %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected no-store cache control on 429, got %q", got)
	}

	// A different source IP is not affected by the exhausted bucket.
	otherSource := httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=10", nil)
	otherSource.RemoteAddr = "203.0.113.99:4444"
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, otherSource)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected other source to pass, got %d", recorder.Code)
	}
}

func TestServerRateLimitsTelemetry(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: &memoryTelemetrySink{}})

	status := 0
	for index := 0; index < telemetryBurst+1; index++ {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", nil))
		status = recorder.Code
		if index < telemetryBurst && status == http.StatusTooManyRequests {
			t.Fatalf("request %d inside burst unexpectedly limited", index+1)
		}
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 past the burst, got %d", status)
	}
}
