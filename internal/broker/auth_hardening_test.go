package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorizedRejectsInvalidTokens(t *testing.T) {
	withAuth := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", nil)
		if v != "" {
			r.Header.Set("Authorization", v)
		}
		return r
	}

	// Empty configured token: open mode (gated at startup), authorize anything.
	if !authorized(withAuth(""), "") {
		t.Fatal("empty configured token should authorize any request")
	}
	// Correct bearer token authorizes.
	if !authorized(withAuth("Bearer s3cret"), "s3cret") {
		t.Fatal("correct bearer token should authorize")
	}
	// Everything else is rejected, including length-adjacent and case variants.
	for _, bad := range []string{"", "Bearer wrong", "s3cret", "Bearer s3cre", "Bearer s3crett", "bearer s3cret", "Bearer  s3cret"} {
		if authorized(withAuth(bad), "s3cret") {
			t.Errorf("authorized accepted invalid Authorization header %q", bad)
		}
	}
}

// TestServerRateLimitsRegister confirms the relay registration endpoint is
// behind the per-IP limiter (the pre-fix endpoint was unthrottled).
func TestServerRateLimitsRegister(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	status := 0
	for i := 0; i < relayRegistrationBurst+1; i++ {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", nil))
		status = recorder.Code
		if i < relayRegistrationBurst && status == http.StatusTooManyRequests {
			t.Fatalf("request %d inside burst unexpectedly limited", i+1)
		}
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 past the burst, got %d", status)
	}
}
