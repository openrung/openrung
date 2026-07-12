package vpnservice

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/internal/relay"
)

func TestDirectoryCacheThrottlesWithinInterval(t *testing.T) {
	var calls int
	var mu sync.Mutex
	now := time.Now()
	d := &directoryCache{
		now: func() time.Time { return now },
		fetcher: func(_ context.Context, _ discovery.Options) (relay.ListResponse, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return relay.ListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []relay.Descriptor{{ID: "r1"}}}, nil
		},
	}

	if _, err := d.fetch(context.Background(), discovery.Options{}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// A second call inside the interval must be served from cache, not refetched.
	now = now.Add(config.MinDirectoryRefreshInterval / 2)
	if _, err := d.fetch(context.Background(), discovery.Options{}); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 broker call within interval, got %d", calls)
	}

	// Past the interval, a refetch is allowed.
	now = now.Add(config.MinDirectoryRefreshInterval)
	if _, err := d.fetch(context.Background(), discovery.Options{}); err != nil {
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
		fetcher: func(_ context.Context, _ discovery.Options) (relay.ListResponse, error) {
			if fail {
				return relay.ListResponse{}, errors.New("broker unreachable")
			}
			return relay.ListResponse{Count: 1, NotAfter: now.Add(time.Hour), Relays: []relay.Descriptor{{ID: "cached"}}}, nil
		},
	}

	if _, err := d.fetch(context.Background(), discovery.Options{}); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// Interval elapses and the broker now fails: the last good list stands in.
	now = now.Add(config.MinDirectoryRefreshInterval + time.Second)
	fail = true
	got, err := d.fetch(context.Background(), discovery.Options{})
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
		fetcher: func(_ context.Context, _ discovery.Options) (relay.ListResponse, error) {
			if fail {
				return relay.ListResponse{}, errors.New("broker unreachable")
			}
			return relay.ListResponse{
				Count:    1,
				NotAfter: now.Add(time.Minute),
				Relays:   []relay.Descriptor{{ID: "expired"}},
			}, nil
		},
	}

	if _, err := d.fetch(context.Background(), discovery.Options{}); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// Once not_after plus the protocol's clock-skew allowance has elapsed, a
	// broker failure must surface instead of resurrecting the signed snapshot.
	now = now.Add(time.Minute + directoryNotAfterSkewAllowance + time.Second)
	fail = true
	if _, err := d.fetch(context.Background(), discovery.Options{}); err == nil {
		t.Fatal("expected broker error after cached snapshot expired")
	}
}

func TestDirectoryCacheErrorsWithoutCache(t *testing.T) {
	d := &directoryCache{
		now: time.Now,
		fetcher: func(_ context.Context, _ discovery.Options) (relay.ListResponse, error) {
			return relay.ListResponse{}, errors.New("broker unreachable")
		},
	}
	if _, err := d.fetch(context.Background(), discovery.Options{}); err == nil {
		t.Fatal("expected error when no cached list exists")
	}
}
