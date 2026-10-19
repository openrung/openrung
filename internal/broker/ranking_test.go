package broker

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestRelayScoreReliabilityDominatesHeadroom(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}

	// Force reliability and capacity into direct conflict: the reliable relay is
	// at twice its advertised capacity (the maximum overload penalty) while the
	// failing relay is idle. Reliability must still win or a small default page
	// excludes the relay that clients can actually use.
	reliable := RelayMetricsSnapshot{
		ActiveSessions: 16,
		Successes:      8,
		Failures:       2,
	}
	failing := RelayMetricsSnapshot{Successes: 2, Failures: 8}

	reliableScore := relayScore(desc, reliable)
	failingScore := relayScore(desc, failing)
	if reliableScore <= failingScore {
		t.Fatalf("reliable overloaded relay score %f must exceed idle failing relay score %f", reliableScore, failingScore)
	}
}

func TestRelayScoreOverCapacityPenaltyIsContinuousAndBounded(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}
	for _, tc := range []struct {
		active int
		want   float64
	}{
		{active: 7, want: 0.550},
		{active: 8, want: 0.525},
		{active: 9, want: 0.520},
		{active: 12, want: 0.505},
		{active: 16, want: 0.485},
		{active: 24, want: 0.485},
	} {
		snapshot := RelayMetricsSnapshot{ActiveSessions: tc.active, Successes: 8, Failures: 2}
		if got := relayScore(desc, snapshot); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("active sessions %d: score = %.9f, want %.9f", tc.active, got, tc.want)
		}
	}
}

func TestRelayScoreOverCapacityPenaltyNeverMakesScoreNegative(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}
	snapshot := RelayMetricsSnapshot{
		ActiveSessions:    16,
		Failures:          1000,
		TCPMS:             metricValue{total: 2000, count: 1},
		SpeedTests:        1,
		DownloadMbpsTotal: 0,
	}
	if got := relayScore(desc, snapshot); got != 0 {
		t.Fatalf("score = %f, want zero floor", got)
	}
}

func TestRelayScoreUnlimitedCapacityHasNoOverloadPenalty(t *testing.T) {
	desc := relay.Descriptor{MaxMbps: 20}
	metrics := RelayMetricsSnapshot{Successes: 8, Failures: 2}
	idleScore := relayScore(desc, metrics)
	metrics.ActiveSessions = 1000
	if busyScore := relayScore(desc, metrics); busyScore != idleScore {
		t.Fatalf("unlimited relay score changed with active sessions: idle %f, busy %f", idleScore, busyScore)
	}
}

// TestRelaySelectionWeightOperatorWeightIsLinear pins the weight's place in
// the formula: the sharpening exponent applies to the telemetry score only, so
// the operator weight scales a relay's sampling weight linearly. Weight 1
// changes nothing, 0.5 halves it, and 0 zeroes it.
func TestRelaySelectionWeightOperatorWeightIsLinear(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 10, MaxMbps: 100}
	overloaded := RelayMetricsSnapshot{ActiveSessions: 15, Successes: 8, Failures: 2}
	score := relayScore(desc, overloaded)
	base := relaySelectionWeight(score, defaultRankingWeight)
	if base <= 0 {
		t.Fatalf("base selection weight must be positive, got %v", base)
	}
	if want := math.Pow(score, rankingSelectionSharpness); math.Abs(base-want) > 1e-12 {
		t.Errorf("unweighted selection weight = %v, want score^sharpness %v", base, want)
	}
	if got, want := relaySelectionWeight(score, 0.5), base*0.5; math.Abs(got-want) > 1e-12 {
		t.Errorf("weight 0.5 = %v, want half of %v", got, base)
	}
	if got := relaySelectionWeight(score, 0); got != 0 {
		t.Errorf("weight 0 = %v, want 0", got)
	}
	// A corrupted backend value cannot inflate a relay past the clamp.
	if got := relaySelectionWeight(score, 4); got != base {
		t.Errorf("weight above 1 = %v, want it clamped to the unweighted %v", got, base)
	}
}

func TestRankingWeightForResolvesDefaultsAndCorruption(t *testing.T) {
	// Only direct database corruption can store NaN, but a NaN weight would
	// otherwise reach the inventory's hand-built JSON as an unparseable
	// literal; the resolver maps it to the default instead.
	if got := rankingWeightFor(map[string]float64{"relay_x": math.NaN()}, "relay_x"); got != defaultRankingWeight {
		t.Errorf("NaN weight resolved to %v, want the default", got)
	}
}

// seededUniform is a reproducible stand-in for rankingUniform.
func seededUniform() func() float64 {
	return rand.New(rand.NewPCG(1, 2)).Float64
}

// constantUniform draws every key from the same U, which reduces the sampled
// order to plain best-selection-weight-first.
func constantUniform() float64 { return 0.5 }

// pinRankingOrder makes the stores rank best-first for the rest of the test,
// for tests that assert the score order itself rather than its spread.
func pinRankingOrder(t *testing.T) {
	t.Helper()
	previous := rankingUniform
	rankingUniform = constantUniform
	t.Cleanup(func() { rankingUniform = previous })
}

// leadShares sorts relays trials times and returns how often each relay ID
// heads the result, as a fraction of trials.
func leadShares(relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot, weights map[string]float64, trials int) map[string]float64 {
	uniform := seededUniform()
	counts := make(map[string]int)
	for range trials {
		page := append([]relay.Descriptor(nil), relays...)
		sortRelayCandidates(page, snapshots, weights, uniform)
		counts[page[0].ID]++
	}
	shares := make(map[string]float64, len(counts))
	for id, count := range counts {
		shares[id] = float64(count) / float64(trials)
	}
	return shares
}

// TestSortRelayCandidatesConstantUniformIsScoreOrder: with a constant source
// the sampled order collapses to best score first, falling back to the
// heartbeat, IPv6 and ID tie-breaks for identical scores.
func TestSortRelayCandidatesConstantUniformIsScoreOrder(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	relays := []relay.Descriptor{
		{ID: "relay_c", PublicHost: "203.0.113.3", MaxSessions: 100, LastHeartbeatAt: now},
		{ID: "relay_b", PublicHost: "203.0.113.2", MaxSessions: 100, LastHeartbeatAt: now},
		{ID: "relay_a", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now},
		{ID: "relay_v6", PublicHost: "2001:db8::1", MaxSessions: 100, LastHeartbeatAt: now},
	}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_c": {Successes: 50},
		"relay_b": {Successes: 10, Failures: 10},
	}
	sortRelayCandidates(relays, snapshots, nil, constantUniform)
	var got []string
	for _, desc := range relays {
		got = append(got, desc.ID)
	}
	// relay_a and relay_v6 share the no-telemetry score; IPv6 wins that tie.
	want := []string{"relay_c", "relay_v6", "relay_a", "relay_b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestSortRelayCandidatesSpreadsNearEqualRelays: two relays that are both past
// capacity and slow differ only slightly in success rate. A best-first sort
// would send every page head to the marginally better one; sampling must give
// each a substantial share, while a clearly weaker relay rarely leads.
func TestSortRelayCandidatesSpreadsNearEqualRelays(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	relays := []relay.Descriptor{
		{ID: "relay_a", PublicHost: "203.0.113.1", MaxSessions: 75, MaxMbps: 100, LastHeartbeatAt: now},
		{ID: "relay_b", PublicHost: "203.0.113.2", MaxSessions: 75, MaxMbps: 100, LastHeartbeatAt: now},
		{ID: "relay_weak", PublicHost: "203.0.113.3", MaxSessions: 75, MaxMbps: 100, LastHeartbeatAt: now},
	}
	slow := metricValue{total: 3000, count: 1}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_a":    {ActiveSessions: 400, Successes: 95, Failures: 5, TCPMS: slow},
		"relay_b":    {ActiveSessions: 400, Successes: 90, Failures: 10, TCPMS: slow},
		"relay_weak": {ActiveSessions: 400, Successes: 40, Failures: 60, TCPMS: slow},
	}
	shares := leadShares(relays, snapshots, nil, 20000)
	for _, id := range []string{"relay_a", "relay_b"} {
		if shares[id] < 0.30 {
			t.Errorf("%s leads %.1f%% of pages, want a substantial share (>= 30%%); shares %v", id, 100*shares[id], shares)
		}
	}
	if shares["relay_a"] <= shares["relay_b"] {
		t.Errorf("the slightly better relay should still lead more often: shares %v", shares)
	}
	if shares["relay_weak"] > 0.02 {
		t.Errorf("clearly weaker relay leads %.1f%% of pages, want rare (<= 2%%)", 100*shares["relay_weak"])
	}
}

// TestSortRelayCandidatesRankingWeightZeroSortsLast: a drained relay stays in
// the list but below every unweighted relay, however weak the latter's
// telemetry, on every draw.
func TestSortRelayCandidatesRankingWeightZeroSortsLast(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	strong := relay.Descriptor{ID: "relay_strong", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now}
	weak := relay.Descriptor{ID: "relay_weak", PublicHost: "203.0.113.2", MaxSessions: 100, LastHeartbeatAt: now}
	other := relay.Descriptor{ID: "relay_other", PublicHost: "203.0.113.3", MaxSessions: 100, LastHeartbeatAt: now}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_strong": {Successes: 50},
		"relay_weak":   {Successes: 1, Failures: 20, ActiveSessions: 100},
	}

	relays := []relay.Descriptor{weak, strong, other}
	sortRelayCandidates(relays, snapshots, nil, constantUniform)
	if relays[0].ID != "relay_strong" {
		t.Fatalf("unweighted: strong relay should lead, got %q", relays[0].ID)
	}

	uniform := seededUniform()
	for range 1000 {
		relays = []relay.Descriptor{strong, weak, other}
		sortRelayCandidates(relays, snapshots, map[string]float64{"relay_strong": 0}, uniform)
		if relays[len(relays)-1].ID != "relay_strong" {
			t.Fatalf("weight 0 must sort the strong relay last, got order %q, %q, %q", relays[0].ID, relays[1].ID, relays[2].ID)
		}
	}
}

// TestSortRelayCandidatesRankingWeightScalesLeadShare: between two relays with
// identical telemetry, an operator weight w gives the weighted relay a lead
// share of w/(1+w), so the dial moves load in proportion.
func TestSortRelayCandidatesRankingWeightScalesLeadShare(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	relays := []relay.Descriptor{
		{ID: "relay_a", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now},
		{ID: "relay_b", PublicHost: "203.0.113.2", MaxSessions: 100, LastHeartbeatAt: now},
	}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_a": {Successes: 50},
		"relay_b": {Successes: 50},
	}
	for _, weight := range []float64{1, 0.5, 0.2} {
		shares := leadShares(relays, snapshots, map[string]float64{"relay_a": weight}, 20000)
		if want := weight / (1 + weight); math.Abs(shares["relay_a"]-want) > 0.02 {
			t.Errorf("weight %v: relay_a leads %.3f of pages, want about %.3f", weight, shares["relay_a"], want)
		}
	}
}
