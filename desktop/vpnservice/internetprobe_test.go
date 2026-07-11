package vpnservice

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/desktop/config"
)

// probeTarget serves as both the HTTP proxy and the origin: with an http://
// probe URL the transport sends an absolute-form GET to the proxy address, so
// a single httptest server exercises the full proxied request path.
func probeTarget(t *testing.T, handler http.HandlerFunc) (port int, url string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	p, err := net.LookupPort("tcp", portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return p, srv.URL + "/generate_204"
}

func swapProbeURLs(t *testing.T, urls ...string) {
	t.Helper()
	restore := config.InternetProbeURLs
	config.InternetProbeURLs = urls
	t.Cleanup(func() { config.InternetProbeURLs = restore })
}

func TestVerifyInternetViaProxyAccepts2xx(t *testing.T) {
	var sawNoCache bool
	port, url := probeTarget(t, func(w http.ResponseWriter, r *http.Request) {
		sawNoCache = r.Header.Get("Cache-Control") == "no-cache"
		w.WriteHeader(http.StatusNoContent)
	})
	swapProbeURLs(t, url)

	ms, err := verifyInternetViaProxy(context.Background(), port)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if ms < 0 {
		t.Fatalf("probe duration = %d", ms)
	}
	if !sawNoCache {
		t.Fatal("probe request missing Cache-Control: no-cache")
	}
}

func TestVerifyInternetViaProxyRejectsRedirects(t *testing.T) {
	port, url := probeTarget(t, func(w http.ResponseWriter, r *http.Request) {
		// A captive portal answering probes with a redirect must not count as
		// internet access.
		http.Redirect(w, r, "http://portal.example/login", http.StatusFound)
	})
	swapProbeURLs(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := verifyInternetViaProxy(ctx, port); err == nil {
		t.Fatal("redirect answer must fail the probe")
	}
}

func TestHealthSweepViaProxySingleSweep(t *testing.T) {
	var calls int
	port, url := probeTarget(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	swapProbeURLs(t, url)

	if err := healthSweepViaProxy(context.Background(), port); err == nil {
		t.Fatal("5xx must fail the health sweep")
	}
	if calls != 1 {
		t.Fatalf("health sweep made %d requests, want exactly 1 (no retry loop)", calls)
	}
}
