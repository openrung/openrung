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

func TestServerRateLimitsSpeedTest(t *testing.T) {
	server := NewServer(NewStore(), Config{})

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
	server := NewServer(NewStore(), Config{TelemetrySink: &memoryTelemetrySink{}})

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
