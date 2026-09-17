package broker

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
)

// The ranking-weight store contract runs against both backends through the
// identity harness: memory always, Postgres when OPENRUNG_TEST_POSTGRES_URL is set.

func TestStoreRankingWeightSetListDelete(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		ctx := context.Background()
		const id = "relay_0123456789abcdef0123456789abcdef"

		weights, err := store.RelayRankingWeights(ctx)
		if err != nil {
			t.Fatalf("list weights: %v", err)
		}
		if len(weights) != 0 {
			t.Fatalf("fresh store has overrides: %v", weights)
		}

		previous, err := store.SetRelayRankingWeight(ctx, id, 0.5)
		if err != nil {
			t.Fatalf("set weight: %v", err)
		}
		if previous != defaultRankingWeight {
			t.Errorf("first set replaced %v, want the default %v", previous, defaultRankingWeight)
		}
		previous, err = store.SetRelayRankingWeight(ctx, id, 0)
		if err != nil {
			t.Fatalf("set weight to 0: %v", err)
		}
		if previous != 0.5 {
			t.Errorf("second set replaced %v, want 0.5", previous)
		}
		weights, err = store.RelayRankingWeights(ctx)
		if err != nil {
			t.Fatalf("list weights: %v", err)
		}
		if len(weights) != 1 || weights[id] != 0 {
			t.Fatalf("weights = %v, want {%s: 0}", weights, id)
		}

		// Out-of-range values are refused, and the stored value is untouched.
		for _, bad := range []float64{-0.01, 1.01, math.NaN(), math.Inf(1)} {
			if _, err := store.SetRelayRankingWeight(ctx, id, bad); !errors.Is(err, ErrInvalidRankingWeight) {
				t.Errorf("set weight %v: err = %v, want ErrInvalidRankingWeight", bad, err)
			}
		}
		weights, _ = store.RelayRankingWeights(ctx)
		if weights[id] != 0 {
			t.Errorf("rejected set changed the stored weight to %v", weights[id])
		}

		previous, err = store.DeleteRelayRankingWeight(ctx, id)
		if err != nil {
			t.Fatalf("delete weight: %v", err)
		}
		if previous != 0 {
			t.Errorf("delete replaced %v, want 0", previous)
		}
		// Deleting an absent override is an idempotent no-op.
		previous, err = store.DeleteRelayRankingWeight(ctx, id)
		if err != nil {
			t.Fatalf("delete absent weight: %v", err)
		}
		if previous != defaultRankingWeight {
			t.Errorf("absent delete replaced %v, want the default", previous)
		}
		weights, _ = store.RelayRankingWeights(ctx)
		if len(weights) != 0 {
			t.Errorf("weights after delete = %v, want none", weights)
		}
	})
}

// A weight must outlive the descriptor it names: pruning the expired lease
// and re-registering the same identity (new endpoint, new lease token) leaves
// the override in force and the relay demoted.
func TestStoreRankingWeightSurvivesPruneAndReRegistration(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		ctx := context.Background()
		now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

		weighted, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, func(req *relay.RegisterRequest) {
			req.PublicHost = "203.0.113.10"
		}, now), now, time.Minute)
		if err != nil {
			t.Fatalf("register weighted relay: %v", err)
		}
		other, err := store.Register(signedIdentityRequest(t, identityStoreSeedB, func(req *relay.RegisterRequest) {
			req.PublicHost = "203.0.113.11"
		}, now), now, time.Minute)
		if err != nil {
			t.Fatalf("register other relay: %v", err)
		}
		// Identical telemetry (none) and heartbeat: only the weight separates them.
		if _, err := store.SetRelayRankingWeight(ctx, weighted.ID, 0); err != nil {
			t.Fatalf("set weight: %v", err)
		}
		listed, err := store.List(now.Add(time.Second), 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != 2 || listed[1].ID != weighted.ID {
			t.Fatalf("weight 0 relay should list last, got %v", relayIDs(listed))
		}

		// The lease expires and is pruned; the override must not go with it.
		later := now.Add(2 * time.Minute)
		if _, err := store.Prune(later); err != nil {
			t.Fatalf("prune: %v", err)
		}
		weights, err := store.RelayRankingWeights(ctx)
		if err != nil {
			t.Fatalf("list weights: %v", err)
		}
		if weights[weighted.ID] != 0 {
			t.Fatalf("prune dropped the override: %v", weights)
		}

		// The same identity re-registers from a new endpoint: same ID, same weight.
		reregistered, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, func(req *relay.RegisterRequest) {
			req.PublicHost = "203.0.113.20"
		}, later), later, time.Minute)
		if err != nil {
			t.Fatalf("re-register weighted relay: %v", err)
		}
		if reregistered.ID != weighted.ID {
			t.Fatalf("identity re-registration changed the ID: %q vs %q", reregistered.ID, weighted.ID)
		}
		if _, err := store.Register(signedIdentityRequest(t, identityStoreSeedB, func(req *relay.RegisterRequest) {
			req.PublicHost = "203.0.113.11"
		}, later), later, time.Minute); err != nil {
			t.Fatalf("re-register other relay: %v", err)
		}
		listed, err = store.List(later.Add(time.Second), 0)
		if err != nil {
			t.Fatalf("list after re-registration: %v", err)
		}
		if len(listed) != 2 || listed[0].ID != other.ID || listed[1].ID != weighted.ID {
			t.Fatalf("re-registered weighted relay should still list last, got %v", relayIDs(listed))
		}

		// Restoring the default lifts the demotion on the next List.
		if _, err := store.DeleteRelayRankingWeight(ctx, weighted.ID); err != nil {
			t.Fatalf("delete weight: %v", err)
		}
		listed, err = store.List(later.Add(time.Second), 0)
		if err != nil {
			t.Fatalf("list after delete: %v", err)
		}
		// Equal scores now: the tie-break is heartbeat (equal) then IPv6 (both
		// v4) then ID, so the order is by ID — whichever it is, the weighted
		// relay is no longer forced last.
		if len(listed) != 2 {
			t.Fatalf("expected 2 relays, got %v", relayIDs(listed))
		}
		if (weighted.ID < other.ID) != (listed[0].ID == weighted.ID) {
			t.Fatalf("after restoring the default, order should fall back to relay ID, got %v", relayIDs(listed))
		}
	})
}

func relayIDs(relays []relay.Descriptor) []string {
	ids := make([]string, 0, len(relays))
	for _, desc := range relays {
		ids = append(ids, desc.ID)
	}
	return ids
}

// Concurrent setters on one relay must each report the exact value they
// replaced: the previous values, taken together, are the default plus every
// written value except the one that ended up stored — a perfect chain. A
// snapshot-read implementation lets two writers report the same predecessor.
func TestStoreRankingWeightConcurrentSettersReportExactPrevious(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		ctx := context.Background()
		const id = "relay_fedcba9876543210fedcba9876543210"
		const writers = 16
		values := make([]float64, writers)
		for i := range values {
			values[i] = float64(i+1) / float64(writers+1)
		}
		previous := make([]float64, writers)
		errs := make([]error, writers)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < writers; i++ {
			done.Add(1)
			go func(i int) {
				defer done.Done()
				start.Wait()
				previous[i], errs[i] = store.SetRelayRankingWeight(ctx, id, values[i])
			}(i)
		}
		start.Done()
		done.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
		}
		weights, err := store.RelayRankingWeights(ctx)
		if err != nil {
			t.Fatalf("list weights: %v", err)
		}
		final := weights[id]

		// Expected multiset: the default once, then every written value
		// except the final one, each exactly once.
		expected := map[float64]int{defaultRankingWeight: 1}
		for _, value := range values {
			if value != final {
				expected[value]++
			}
		}
		got := map[float64]int{}
		for _, value := range previous {
			got[value]++
		}
		if len(got) != len(expected) {
			t.Fatalf("previous values %v do not chain from %v to %v", previous, defaultRankingWeight, final)
		}
		for value, count := range expected {
			if got[value] != count {
				t.Fatalf("previous value %v reported %d times, want %d (previous=%v, final=%v)", value, got[value], count, previous, final)
			}
		}
	})
}
