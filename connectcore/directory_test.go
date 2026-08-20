package connectcore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/discovery"
)

func TestDirectoryCacheThrottlesWithinInterval(t *testing.T) {
	var calls int
	var mu sync.Mutex
	now := time.Now()
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return brokerapi.RelayListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []brokerapi.RelayDescriptor{{ID: "r1"}}}, nil
		},
	}

	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// A second call inside the interval must be served from cache, not refetched.
	now = now.Add(MinDirectoryRefreshInterval / 2)
	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 broker call within interval, got %d", calls)
	}

	// Past the interval, a refetch is allowed.
	now = now.Add(MinDirectoryRefreshInterval)
	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 broker calls after interval, got %d", calls)
	}
}

func TestDirectoryCacheServesStaleOnError(t *testing.T) {
	now := time.Now()
	fail := false
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			if fail {
				return brokerapi.RelayListResponse{}, errors.New("broker unreachable")
			}
			return brokerapi.RelayListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []brokerapi.RelayDescriptor{{ID: "cached"}}}, nil
		},
	}

	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// Interval elapses and the broker now fails: the last good list stands in.
	now = now.Add(MinDirectoryRefreshInterval + time.Second)
	fail = true
	got, err := d.fetch(context.Background(), "", discovery.Options{})
	if err != nil {
		t.Fatalf("expected stale-serve, got error: %v", err)
	}
	if len(got.Relays) != 1 || got.Relays[0].ID != "cached" {
		t.Fatalf("expected cached relay, got %+v", got.Relays)
	}
}

func TestDirectoryCacheRefusesExpiredSnapshotOnError(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	fail := false
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			if fail {
				return brokerapi.RelayListResponse{}, errors.New("broker unreachable")
			}
			return brokerapi.RelayListResponse{
				Count:    1,
				NotAfter: now.Add(time.Minute),
				Relays:   []brokerapi.RelayDescriptor{{ID: "expired"}},
			}, nil
		},
	}

	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// Once not_after plus the protocol's clock-skew allowance has elapsed, a
	// broker failure must surface instead of resurrecting the signed snapshot.
	now = now.Add(time.Minute + directoryNotAfterSkewAllowance + time.Second)
	fail = true
	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err == nil {
		t.Fatal("expected broker error after cached snapshot expired")
	}
}

func TestDirectoryCacheErrorsWithoutCache(t *testing.T) {
	d := &directoryCache{
		now: time.Now,
		fetcher: func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			return brokerapi.RelayListResponse{}, errors.New("broker unreachable")
		},
	}
	if _, err := d.fetch(context.Background(), "", discovery.Options{}); err == nil {
		t.Fatal("expected error when no cached list exists")
	}
}

func TestRankedDirectoryOrdersUsableRelaysByLatency(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())

	now := time.Now()
	slow := brokerapi.RelayDescriptor{
		ID: "slow", PublicHost: "host-slow", PublicPort: 443,
		Protocol: brokerapi.ProtocolVLESSRealityVision, ClientID: "uuid",
		RealityPublicKey: "pk", ShortID: "sid", ServerName: "sni",
		Flow: brokerapi.FlowVision, ExitMode: brokerapi.ExitModeDirect,
		ExpiresAt: now.Add(time.Hour),
	}
	fast := slow
	fast.ID, fast.PublicHost = "fast", "host-fast"
	expired := slow
	expired.ID, expired.ExpiresAt = "expired", now.Add(-time.Hour)

	s := New()
	s.directory.fetcher = func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
		return brokerapi.RelayListResponse{
			Count:      3,
			ServerTime: now,
			NotAfter:   now.Add(time.Hour),
			Relays:     []brokerapi.RelayDescriptor{slow, fast, expired},
		}, nil
	}
	s.dialRelay = func(_ context.Context, host string, _ int) (int64, error) {
		if host == "host-fast" {
			return 3, nil
		}
		return 300, nil
	}

	ranked, err := s.RankedDirectory(context.Background(), "")
	if err != nil {
		t.Fatalf("RankedDirectory: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("ranked = %d entries, want 2 (the expired relay is not usable)", len(ranked))
	}
	if ranked[0].Relay.ID != "fast" || ranked[1].Relay.ID != "slow" {
		t.Fatalf("ranked order = [%s %s], want the measured-fast relay first", ranked[0].Relay.ID, ranked[1].Relay.ID)
	}
	if ranked[0].ProbeMS == nil || *ranked[0].ProbeMS != 3 {
		t.Fatalf("fast probe = %v, want 3ms", ranked[0].ProbeMS)
	}
}

func TestDirectoryCacheKeyedByBrokerOverride(t *testing.T) {
	// A fresh snapshot from one broker must not answer a request for another:
	// a cross-broker cache hit would point targeted connects at relays the
	// requested broker never listed.
	now := time.Now()
	var urls []string
	var mu sync.Mutex
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, brokerURL string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			mu.Lock()
			urls = append(urls, brokerURL)
			mu.Unlock()
			return brokerapi.RelayListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []brokerapi.RelayDescriptor{{ID: "from-" + brokerURL}}}, nil
		},
	}

	if _, err := d.fetch(context.Background(), "a", discovery.Options{}); err != nil {
		t.Fatalf("fetch a: %v", err)
	}
	resp, err := d.fetch(context.Background(), "b", discovery.Options{})
	if err != nil {
		t.Fatalf("fetch b: %v", err)
	}
	if len(urls) != 2 || urls[1] != "b" {
		t.Fatalf("fetcher calls = %v, want a refetch for the new broker", urls)
	}
	if resp.Relays[0].ID != "from-b" {
		t.Fatalf("served %q, want the new broker's list", resp.Relays[0].ID)
	}

	// The error fallback must not serve broker b's snapshot for broker c either.
	d.fetcher = func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
		return brokerapi.RelayListResponse{}, errors.New("broker unreachable")
	}
	if _, err := d.fetch(context.Background(), "c", discovery.Options{}); err == nil {
		t.Fatal("fetch c served a different broker's stale snapshot")
	}
}

func TestDirectoryCacheSingleFlight(t *testing.T) {
	// Concurrent refreshes must collapse onto one broker request: the TUI's
	// startup refresh and an eager r press race exactly like this.
	now := time.Now()
	var calls int32
	release := make(chan struct{})
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, _ string, _ discovery.Options) (brokerapi.RelayListResponse, error) {
			atomic.AddInt32(&calls, 1)
			<-release
			return brokerapi.RelayListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []brokerapi.RelayDescriptor{{ID: "r1"}}}, nil
		},
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.fetch(context.Background(), "", discovery.Options{}); err != nil {
				t.Errorf("fetch: %v", err)
			}
		}()
	}
	// Let every goroutine reach the cache before the one in-flight fetch lands.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("broker calls = %d, want 1: latecomers must wait for the in-flight fetch", got)
	}
}
