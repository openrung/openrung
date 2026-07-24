package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthURLAppendsToBasePathAndDropsQuery(t *testing.T) {
	tests := []struct {
		name      string
		brokerURL string
		want      string
	}{
		{
			name:      "root",
			brokerURL: "https://broker.openrung.org/?stale=1",
			want:      "https://broker.openrung.org/healthz",
		},
		{
			name:      "base path",
			brokerURL: "https://broker.openrung.org/front/v1/?stale=1",
			want:      "https://broker.openrung.org/front/v1/healthz",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := healthURL(tc.brokerURL)
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tc.brokerURL, err)
			}
			if got != tc.want {
				t.Fatalf("healthURL(%q) = %q, want %q", tc.brokerURL, got, tc.want)
			}
		})
	}
}

func TestProbeInternetRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	if _, ok := probeInternet(t.Context(), server.URL); !ok {
		t.Fatal("probeInternet did not succeed after the transient failure")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe requests = %d, want 2", got)
	}
}

func TestProbeInternetDoesNotFollowRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(redirectTarget.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/healthz", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	if _, ok := probeInternet(t.Context(), origin.URL); !ok {
		t.Fatal("probeInternet rejected the origin's reachable redirect response")
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestProbeInternetCancellationStopsInflightRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	probeDone := make(chan bool, 1)
	go func() {
		_, ok := probeInternet(ctx, server.URL)
		probeDone <- ok
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("probe request did not start")
	}

	cancelledAt := time.Now()
	cancel()
	select {
	case ok := <-probeDone:
		if ok {
			t.Fatal("canceled probe reported success")
		}
		if elapsed := time.Since(cancelledAt); elapsed > time.Second {
			t.Fatalf("canceled probe returned after %v, want under 1s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled probe did not return promptly")
	}
}
