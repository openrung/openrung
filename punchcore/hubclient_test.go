package punchcore

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestHardenedHTTPClientContract pins the security-relevant properties of the
// hardened default the mobile client uses for coordinators without a
// certificate pin: bounded requests, no redirect following (an unauthenticated
// coordinator must not be able to re-point the client at a different host),
// and no connection reuse.
func TestHardenedHTTPClientContract(t *testing.T) {
	client := HardenedHTTPClient()

	if client.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", client.Transport)
	}
	if !transport.DisableKeepAlives {
		t.Error("DisableKeepAlives = false, want true")
	}

	// A redirect must be returned to the caller as-is, never followed.
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirecting := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirecting.Close()

	resp, err := client.Get(redirecting.URL)
	if err != nil {
		t.Fatalf("GET redirecting server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect must surface, not be followed)", resp.StatusCode, http.StatusFound)
	}
	if got := targetHits.Load(); got != 0 {
		t.Errorf("redirect target served %d requests, want 0", got)
	}
}
