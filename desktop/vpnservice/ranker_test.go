package vpnservice

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/internal/relay"
)

// rankRelay builds a usable candidate whose public host identifies it, so a fake
// probe can answer per-relay from a host-keyed table.
func rankRelay(id string) relay.Descriptor {
	return relayAt(id, "JP", "Tokyo", "Japan", "host-"+id)
}

func rankedIDs(ranked []rankedRelay) []string {
	return candidateIDs(ladderOrder{ranked: ranked}.candidates())
}

// probeTable fakes the ranker's probe: a relay in the table measures its
// latency, one absent from it fails to connect.
func probeTable(latencies map[string]int64) func(context.Context, string, int) (int64, error) {
	return func(_ context.Context, host string, _ int) (int64, error) {
		ms, ok := latencies[host]
		if !ok {
			return 0, fmt.Errorf("relay %s is not reachable: connect timed out", host)
		}
		return ms, nil
	}
}

func rank(cands []relay.Descriptor, maxProbes int, probe func(context.Context, string, int) (int64, error)) []rankedRelay {
	return rankByTCPLatency(context.Background(), cands, maxProbes, time.Second, config.RelayRankBucketMS, probe)
}

func wantProbeMS(t *testing.T, r rankedRelay, want int64) {
	t.Helper()
	if r.probeMS == nil {
		t.Fatalf("relay %s probeMS = nil, want %d", r.relay.ID, want)
	}
	if *r.probeMS != want {
		t.Fatalf("relay %s probeMS = %d, want %d", r.relay.ID, *r.probeMS, want)
	}
}

func TestRankByTCPLatencySortsByLatencyBucket(t *testing.T) {
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b"), rankRelay("c")}
	probe := probeTable(map[string]int64{"host-a": 200, "host-b": 40, "host-c": 120})

	ranked := rank(cands, config.RelayRankMaxProbes, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"b", "c", "a"}) {
		t.Fatalf("ranked = %v, want [b c a]", ids)
	}
	wantProbeMS(t, ranked[0], 40)
	wantProbeMS(t, ranked[1], 120)
	wantProbeMS(t, ranked[2], 200)
}

func TestRankByTCPLatencyKeepsBrokerOrderWithinBucket(t *testing.T) {
	// 31 and 45 share the 30ms bucket, so broker order (a before b) must survive
	// even though b measured marginally faster — within a bucket the broker's
	// load balancing still decides. 95 lands two buckets up and sorts last.
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b"), rankRelay("c")}
	probe := probeTable(map[string]int64{"host-a": 45, "host-b": 31, "host-c": 95})

	ranked := rank(cands, config.RelayRankMaxProbes, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ranked = %v, want [a b c]", ids)
	}
}

func TestRankByTCPLatencySinksFailedProbesWithoutDropping(t *testing.T) {
	// Fail-open: a relay whose probe failed sinks below the reachable ones but
	// stays in the ladder — the ladder's own 5s gate may still connect where the
	// 1.5s probe gave up.
	cands := []relay.Descriptor{rankRelay("dead"), rankRelay("slow"), rankRelay("fast")}
	probe := probeTable(map[string]int64{"host-slow": 400, "host-fast": 20})

	ranked := rank(cands, config.RelayRankMaxProbes, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"fast", "slow", "dead"}) {
		t.Fatalf("ranked = %v, want [fast slow dead]", ids)
	}
	if ranked[2].probeMS != nil {
		t.Fatalf("failed relay probeMS = %d, want nil", *ranked[2].probeMS)
	}
}

func TestRankByTCPLatencyAllProbesFailingKeepsBrokerOrder(t *testing.T) {
	// The ranker never makes things worse: if nothing answers, the ladder is the
	// broker's list unchanged.
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b"), rankRelay("c")}

	ranked := rank(cands, config.RelayRankMaxProbes, probeTable(nil))

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ranked = %v, want broker order [a b c]", ids)
	}
}

func TestRankByTCPLatencyKeepsUnprobedTailInBrokerOrder(t *testing.T) {
	cands := []relay.Descriptor{rankRelay("r1"), rankRelay("r2"), rankRelay("r3"), rankRelay("r4"), rankRelay("r5")}
	// Probe only the first three, reversing their latency so the head visibly
	// reorders while the tail must not move.
	probe := probeTable(map[string]int64{"host-r1": 300, "host-r2": 150, "host-r3": 10})

	ranked := rank(cands, 3, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"r3", "r2", "r1", "r4", "r5"}) {
		t.Fatalf("ranked = %v, want [r3 r2 r1 r4 r5]", ids)
	}
	if ranked[3].probeMS != nil || ranked[4].probeMS != nil {
		t.Fatal("unprobed tail carries a probe measurement")
	}
}

func TestRankByTCPLatencySinksFailedProbesAboveTheUnprobedTail(t *testing.T) {
	// A relay we probed and could not reach still outranks one we never probed:
	// the tail may be anything, while a failed probe is only evidence of
	// slowness. Ordering the two groups the other way round would bury the tail
	// behind relays already known to be unreachable.
	cands := []relay.Descriptor{rankRelay("r1"), rankRelay("r2"), rankRelay("r3"), rankRelay("r4"), rankRelay("r5")}
	probe := probeTable(map[string]int64{"host-r2": 150, "host-r3": 10}) // r1 unreachable

	ranked := rank(cands, 3, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"r3", "r2", "r1", "r4", "r5"}) {
		t.Fatalf("ranked = %v, want [r3 r2 r1 r4 r5]: failed probes above the unprobed tail", ids)
	}
}

func TestRankByTCPLatencySingleCandidateSkipsProbe(t *testing.T) {
	var probes int32
	probe := func(context.Context, string, int) (int64, error) {
		atomic.AddInt32(&probes, 1)
		return 10, nil
	}

	ranked := rank([]relay.Descriptor{rankRelay("only")}, config.RelayRankMaxProbes, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"only"}) {
		t.Fatalf("ranked = %v, want [only]", ids)
	}
	if ranked[0].probeMS != nil {
		t.Fatal("single candidate was probed")
	}
	if got := atomic.LoadInt32(&probes); got != 0 {
		t.Fatalf("probes = %d, want 0: nothing to reorder, so nothing to probe", got)
	}
}

func TestRankByTCPLatencyZeroMillisecondProbeRanksAsReachable(t *testing.T) {
	// relayTCPReachable floors to whole milliseconds, so a very close relay
	// really does measure 0. It must rank as the fastest reachable relay, not be
	// mistaken for one that was never probed.
	cands := []relay.Descriptor{rankRelay("far"), rankRelay("near"), rankRelay("dead")}
	probe := probeTable(map[string]int64{"host-far": 90, "host-near": 0})

	ranked := rank(cands, config.RelayRankMaxProbes, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"near", "far", "dead"}) {
		t.Fatalf("ranked = %v, want [near far dead]", ids)
	}
	wantProbeMS(t, ranked[0], 0)
}

func TestRankByTCPLatencyProbesConcurrently(t *testing.T) {
	// Ranking costs the user one probe timeout, not the sum of them. The barrier
	// asserts simultaneity directly: every probe must be in flight before any may
	// return, which a sequential ranker can never satisfy at any duration. There
	// is no elapsed-time assert here, so there is nothing to flake.
	const want = 4
	cands := []relay.Descriptor{rankRelay("r1"), rankRelay("r2"), rankRelay("r3"), rankRelay("r4")}

	arrived := make(chan struct{}, want)
	release := make(chan struct{})
	probe := func(context.Context, string, int) (int64, error) {
		arrived <- struct{}{}
		<-release
		return 10, nil
	}

	done := make(chan []rankedRelay, 1)
	go func() { done <- rank(cands, config.RelayRankMaxProbes, probe) }()

	for i := 0; i < want; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d probes were in flight; probes are running sequentially", i, want)
		}
	}
	close(release)

	select {
	case ranked := <-done:
		if len(ranked) != want {
			t.Fatalf("ranked = %v, want all %d candidates", rankedIDs(ranked), want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ranker never returned after the barrier released")
	}
}

func TestRankByTCPLatencyBoundsEveryProbeByTheRankTimeout(t *testing.T) {
	// A black-holed relay must not stall the connect: the rank probe carries its
	// own deadline rather than inheriting the ladder's longer reachability
	// budget. A probe that only returns when its ctx expires proves the bound —
	// without it this test would hang rather than fail.
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b")}
	probe := func(ctx context.Context, _ string, _ int) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	ranked := rankByTCPLatency(context.Background(), cands, config.RelayRankMaxProbes, time.Millisecond, config.RelayRankBucketMS, probe)

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"a", "b"}) {
		t.Fatalf("ranked = %v, want broker order [a b]", ids)
	}
}

func TestRankByTCPLatencyCancelledContextKeepsEveryCandidate(t *testing.T) {
	// A disconnect racing the ranker must not be read as "these relays are
	// unusable" — the ladder still receives every candidate, and runLadder's own
	// ctx check is what ends the connect.
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b"), rankRelay("c")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ranked := rankByTCPLatency(ctx, cands, config.RelayRankMaxProbes, time.Second, config.RelayRankBucketMS,
		func(ctx context.Context, _ string, _ int) (int64, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})

	if ids := rankedIDs(ranked); !equalIDs(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ranked = %v, want every candidate in broker order", ids)
	}
}

func TestRankLadderSkipsPinnedRelay(t *testing.T) {
	// The guard keys off the pinned id, not the list length, mirroring mobile: a
	// relay the user chose is never reordered, however many candidates the filter
	// happened to return.
	s := New()
	var probes int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		atomic.AddInt32(&probes, 1)
		return 1, nil
	}
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b")}

	order := s.rankLadder(context.Background(), cands, "b")

	if ids := candidateIDs(order.candidates()); !equalIDs(ids, []string{"a", "b"}) {
		t.Fatalf("pinned ladder = %v, want broker order [a b]", ids)
	}
	if got := atomic.LoadInt32(&probes); got != 0 {
		t.Fatalf("probes = %d, want 0: a pinned relay is not ranked", got)
	}
}

func TestRankLadderRanksCountryTargetedConnect(t *testing.T) {
	// A country target narrows the set; ranking still applies within it.
	s := New()
	s.dialRelay = probeTable(map[string]int64{"host-a": 200, "host-b": 20})
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b")}

	order := s.rankLadder(context.Background(), cands, "")

	if ids := candidateIDs(order.candidates()); !equalIDs(ids, []string{"b", "a"}) {
		t.Fatalf("ranked ladder = %v, want [b a]", ids)
	}
}

func TestLadderOrderAnnotateReportsBrokerIndexAndProbe(t *testing.T) {
	// The winner's telemetry must say where it sat before the client reordered.
	cands := []relay.Descriptor{rankRelay("a"), rankRelay("b"), rankRelay("c")}
	order := ladderOrder{
		brokerOrder: cands,
		ranked:      rank(cands, config.RelayRankMaxProbes, probeTable(map[string]int64{"host-a": 200, "host-b": 40, "host-c": 120})),
	}

	res := &candidateResult{relay: rankRelay("b"), brokerIndex: -1}
	order.annotate(res)

	if res.brokerIndex != 1 {
		t.Fatalf("brokerIndex = %d, want 1", res.brokerIndex)
	}
	if res.rankProbeMS == nil || *res.rankProbeMS != 40 {
		t.Fatalf("rankProbeMS = %v, want 40", res.rankProbeMS)
	}
}

func TestLadderOrderAnnotateLeavesProbeAbsentForUnrankedLadder(t *testing.T) {
	// A pinned connect is not probed, so it reports its broker position and no
	// probe measurement — never a fabricated zero.
	cands := []relay.Descriptor{rankRelay("a")}
	order := ladderOrder{brokerOrder: cands, ranked: unranked(cands)}

	res := &candidateResult{relay: rankRelay("a"), brokerIndex: -1}
	order.annotate(res)

	if res.brokerIndex != 0 {
		t.Fatalf("brokerIndex = %d, want 0", res.brokerIndex)
	}
	if res.rankProbeMS != nil {
		t.Fatalf("rankProbeMS = %d, want nil for an unranked ladder", *res.rankProbeMS)
	}
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
