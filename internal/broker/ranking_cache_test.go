// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

type countingRankingLoader struct {
	loads  atomic.Int32
	relays []relay.Descriptor
	err    error
}

func (l *countingRankingLoader) load(time.Time) ([]relay.Descriptor, RankingInputs, error) {
	l.loads.Add(1)
	if l.err != nil {
		return nil, RankingInputs{}, l.err
	}
	return append([]relay.Descriptor(nil), l.relays...), RankingInputs{Weights: map[string]float64{}}, nil
}

func rankingCacheFixture(now time.Time) *countingRankingLoader {
	return &countingRankingLoader{relays: []relay.Descriptor{
		{ID: "a", ClientID: "ca", ExpiresAt: now.Add(time.Minute)},
		{ID: "b", ClientID: "cb", ExpiresAt: now.Add(2 * time.Second)},
	}}
}

func TestRankingCacheReusesLoadWithinTTL(t *testing.T) {
	now := time.Now().UTC()
	loader := rankingCacheFixture(now)
	cache := &rankingCache{ttl: time.Minute}

	first, _, err := cache.get(now, loader.load)
	if err != nil || len(first) != 2 {
		t.Fatalf("first get = %v, %v", first, err)
	}
	first[0], first[1] = first[1], first[0] // callers sort in place
	second, _, err := cache.get(now.Add(time.Second), loader.load)
	if err != nil {
		t.Fatal(err)
	}
	if got := loader.loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}
	if second[0].ID != "a" {
		t.Fatalf("a caller's reorder leaked into the cache: %v", second)
	}

	// Expiry is applied per request against the caller's time.
	later, _, err := cache.get(now.Add(3*time.Second), loader.load)
	if err != nil || len(later) != 1 || later[0].ID != "a" {
		t.Fatalf("get after b expired = %v, %v", later, err)
	}
}

func TestRankingCacheReloadsOutsideItsWindow(t *testing.T) {
	now := time.Now().UTC()
	loader := rankingCacheFixture(now)
	cache := &rankingCache{ttl: 5 * time.Second}

	for _, at := range []time.Time{now, now.Add(-time.Second), now.Add(-time.Second).Add(5 * time.Second)} {
		if _, _, err := cache.get(at, loader.load); err != nil {
			t.Fatal(err)
		}
	}
	// The earlier time cannot reuse a load that filtered at a later one, and
	// a time a full TTL past the load wants a newer window.
	if got := loader.loads.Load(); got != 3 {
		t.Fatalf("loads = %d, want 3", got)
	}

	disabled := &rankingCache{}
	for range 2 {
		if _, _, err := disabled.get(now, loader.load); err != nil {
			t.Fatal(err)
		}
	}
	if got := loader.loads.Load(); got != 5 {
		t.Fatalf("a zero TTL cached: loads = %d, want 5", got)
	}
}

func TestRankingCacheDoesNotCacheErrors(t *testing.T) {
	now := time.Now().UTC()
	loader := rankingCacheFixture(now)
	loader.err = errors.New("database down")
	cache := &rankingCache{ttl: time.Minute}
	if _, _, err := cache.get(now, loader.load); err == nil {
		t.Fatal("expected the load error")
	}
	loader.err = nil
	if relays, _, err := cache.get(now, loader.load); err != nil || len(relays) != 2 {
		t.Fatalf("get after recovery = %v, %v", relays, err)
	}
	if got := loader.loads.Load(); got != 2 {
		t.Fatalf("loads = %d, want 2", got)
	}
}

func TestRankingCacheInvalidation(t *testing.T) {
	now := time.Now().UTC()
	loader := rankingCacheFixture(now)
	cache := &rankingCache{ttl: time.Minute}
	get := func() {
		t.Helper()
		if _, _, err := cache.get(now, loader.load); err != nil {
			t.Fatal(err)
		}
	}

	get()
	cache.invalidate()
	get()
	if got := loader.loads.Load(); got != 2 {
		t.Fatalf("loads after invalidate = %d, want 2", got)
	}

	cache.invalidateIfChanged("a", func(cached relay.Descriptor) bool { return cached.ClientID != "ca" })
	cache.invalidateIfChanged("missing", func(relay.Descriptor) bool { return true })
	get()
	if got := loader.loads.Load(); got != 2 {
		t.Fatalf("an unchanged or uncached relay invalidated: loads = %d, want 2", got)
	}
	cache.invalidateIfChanged("a", func(cached relay.Descriptor) bool { return cached.ClientID != "rotated" })
	get()
	if got := loader.loads.Load(); got != 3 {
		t.Fatalf("loads after a changed relay = %d, want 3", got)
	}
}

func TestRankingCacheDiscardsLoadRacingAnInvalidation(t *testing.T) {
	now := time.Now().UTC()
	cache := &rankingCache{ttl: time.Minute}
	var loads atomic.Int32
	racing := func(time.Time) ([]relay.Descriptor, RankingInputs, error) {
		if loads.Add(1) == 1 {
			cache.invalidate() // a write commits while the first load runs
		}
		return nil, RankingInputs{}, nil
	}
	for range 2 {
		if _, _, err := cache.get(now, racing); err != nil {
			t.Fatal(err)
		}
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("a load that raced a write was reused: loads = %d, want 2", got)
	}
}

func TestRankingCacheCoalescesConcurrentMisses(t *testing.T) {
	now := time.Now().UTC()
	cache := &rankingCache{ttl: time.Minute}
	release := make(chan struct{})
	var loads atomic.Int32
	slow := func(time.Time) ([]relay.Descriptor, RankingInputs, error) {
		loads.Add(1)
		<-release
		return []relay.Descriptor{{ID: "a", ExpiresAt: now.Add(time.Minute)}}, RankingInputs{}, nil
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if relays, _, err := cache.get(now, slow); err != nil || len(relays) != 1 {
				t.Errorf("get = %v, %v", relays, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}
}
