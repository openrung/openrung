package broker

import (
	"context"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

// defaultNeutralScores are the neutral scores of a ranking pass in which no
// relay is measured.
var defaultNeutralScores = rankingNeutralScores{
	Latency:   rankingDefaultLatencyScore,
	Handshake: rankingDefaultHandshakeScore,
}

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

	reliableScore := relayScore(desc, reliable, defaultNeutralScores)
	failingScore := relayScore(desc, failing, defaultNeutralScores)
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
		if got := relayScore(desc, snapshot, defaultNeutralScores); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("active sessions %d: score = %.9f, want %.9f", tc.active, got, tc.want)
		}
	}
}

func TestRelayScoreOverCapacityPenaltyNeverMakesScoreNegative(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}
	snapshot := RelayMetricsSnapshot{
		ActiveSessions:      16,
		Failures:            1000,
		TCPMS:               2000,
		TCPHandshakes:       rankingHandshakeMinSamples,
		TCPHandshakesResent: rankingHandshakeMinSamples,
	}
	if got := relayScore(desc, snapshot, defaultNeutralScores); got != 0 {
		t.Fatalf("score = %f, want zero floor", got)
	}
}

func TestRelayScoreUnlimitedCapacityHasNoOverloadPenalty(t *testing.T) {
	desc := relay.Descriptor{MaxMbps: 20}
	metrics := RelayMetricsSnapshot{Successes: 8, Failures: 2}
	idleScore := relayScore(desc, metrics, defaultNeutralScores)
	metrics.ActiveSessions = 1000
	if busyScore := relayScore(desc, metrics, defaultNeutralScores); busyScore != idleScore {
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
	score := relayScore(desc, overloaded, defaultNeutralScores)
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
	slow := 3000.0
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

func TestRankRelaysBreaksDownScoreAndHeadShare(t *testing.T) {
	now := time.Now().UTC()
	relays := []relay.Descriptor{
		{ID: "relay-a", MaxSessions: 8, MaxMbps: 20},
		{ID: "relay-b", MaxSessions: 8, MaxMbps: 20},
		{ID: "relay-withheld", MaxSessions: 8, MaxMbps: 20, ReachabilityCheckedAt: now},
	}
	inputs := RankingInputs{
		Weights: map[string]float64{"relay-b": 0.5},
		Snapshots: map[string]RelayMetricsSnapshot{
			"relay-a": {ActiveSessions: 2, Successes: 8, Failures: 2, TCPMS: 300},
		},
	}

	rankings := rankRelays(relays, inputs, true)

	a := rankings["relay-a"]
	parts := relayScoreBreakdown(relays[0], inputs.Snapshots["relay-a"], neutralScores(relays, inputs.Snapshots))
	if a.Score != parts.score() || a.Reliability != parts.Reliability ||
		a.Headroom != parts.Headroom || a.Latency != parts.Latency || a.Handshake != parts.Handshake {
		t.Fatalf("relay-a ranking %+v does not match its score parts %+v", a, parts)
	}
	b := rankings["relay-b"]
	if want := relaySelectionWeight(b.Score, 0.5); b.SelectionWeight != want {
		t.Fatalf("relay-b selection weight = %v, want %v (operator weight applied)", b.SelectionWeight, want)
	}

	withheld := rankings["relay-withheld"]
	if withheld.SelectionWeight <= 0 || withheld.HeadShare != 0 {
		t.Fatalf("withheld relay = %+v, want a selection weight but no head share", withheld)
	}
	if got := a.HeadShare + b.HeadShare; math.Abs(got-1) > 1e-12 {
		t.Fatalf("head shares of listed relays sum to %v, want 1", got)
	}
	if want := a.SelectionWeight / (a.SelectionWeight + b.SelectionWeight); math.Abs(a.HeadShare-want) > 1e-12 {
		t.Fatalf("relay-a head share = %v, want %v", a.HeadShare, want)
	}
}

func TestRankRelaysAllZeroWeightsHaveNoHeadShare(t *testing.T) {
	relays := []relay.Descriptor{{ID: "relay-a"}, {ID: "relay-b"}}
	inputs := RankingInputs{Weights: map[string]float64{"relay-a": 0, "relay-b": 0}}
	for id, ranking := range rankRelays(relays, inputs, true) {
		if ranking.HeadShare != 0 || math.IsNaN(ranking.HeadShare) {
			t.Fatalf("%s head share = %v, want 0", id, ranking.HeadShare)
		}
	}
}

// With probing disabled the public list keeps relays whose stored probe
// failed, so their lead share must count too.
func TestRankRelaysHonoursDisabledProbing(t *testing.T) {
	now := time.Now().UTC()
	relays := []relay.Descriptor{
		{ID: "relay-a", MaxSessions: 8},
		{ID: "relay-stale-failure", MaxSessions: 8, ReachabilityCheckedAt: now},
	}
	rankings := rankRelays(relays, RankingInputs{}, false)
	for _, id := range []string{"relay-a", "relay-stale-failure"} {
		if got := rankings[id].HeadShare; math.Abs(got-0.5) > 1e-12 {
			t.Errorf("%s head_share = %v, want 0.5 with probing disabled", id, got)
		}
	}
}

func TestMeasuredLatencyScoreRampsEachMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot RelayMetricsSnapshot
		want     float64
	}{
		{name: "tcp fast", snapshot: RelayMetricsSnapshot{TCPMS: 100}, want: 1},
		{name: "tcp midway", snapshot: RelayMetricsSnapshot{TCPMS: 550}, want: 0.5},
		{name: "tcp slow", snapshot: RelayMetricsSnapshot{TCPMS: 1000}, want: 0},
		{name: "probe fast", snapshot: RelayMetricsSnapshot{InternetProbeMS: 900}, want: 1},
		{name: "probe midway", snapshot: RelayMetricsSnapshot{InternetProbeMS: 5500}, want: 0.5},
		{name: "probe slow", snapshot: RelayMetricsSnapshot{InternetProbeMS: 12000}, want: 0},
		{name: "average of measured", snapshot: RelayMetricsSnapshot{TCPMS: 100, InternetProbeMS: 5500}, want: 0.75},
		{name: "both", snapshot: RelayMetricsSnapshot{TCPMS: 1000, InternetProbeMS: 1000}, want: 0.5},
	} {
		got, ok := measuredLatencyScore(tc.snapshot)
		if !ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: score = %v (measured %v), want %v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := measuredLatencyScore(RelayMetricsSnapshot{Successes: 5}); ok {
		t.Errorf("a snapshot without latency medians must not count as measured")
	}
}

// tcpLatencyFor returns the TCP connect median whose latency score is score.
func tcpLatencyFor(score float64) float64 {
	return rankingTCPSlowMS - score*(rankingTCPSlowMS-rankingTCPFastMS)
}

func TestNeutralLatencyScoreIsMedianOfMeasuredRelays(t *testing.T) {
	unmeasured := relay.Descriptor{ID: "relay_new"}
	for _, tc := range []struct {
		name   string
		scores []float64
		want   float64
	}{
		{name: "odd", scores: []float64{1, 0.2, 0.8}, want: 0.8},
		{name: "even", scores: []float64{1, 0.2, 0.8, 0.4}, want: 0.6},
		{name: "none", want: rankingDefaultLatencyScore},
	} {
		relays := []relay.Descriptor{unmeasured}
		snapshots := map[string]RelayMetricsSnapshot{
			// Telemetry without latency medians leaves a relay unmeasured.
			unmeasured.ID: {Successes: 3},
			// A relay outside the pass does not move the median.
			"relay_elsewhere": {TCPMS: tcpLatencyFor(0)},
		}
		for i, score := range tc.scores {
			id := "relay_" + string(rune('a'+i))
			relays = append(relays, relay.Descriptor{ID: id})
			snapshots[id] = RelayMetricsSnapshot{TCPMS: tcpLatencyFor(score)}
		}
		if got := neutralScores(relays, snapshots).Latency; math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: neutral latency = %v, want %v", tc.name, got, tc.want)
		}
		if got := rankRelays(relays, RankingInputs{Snapshots: snapshots}, false)[unmeasured.ID].Latency; math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: unmeasured relay latency = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnmeasuredRelayNeitherLeadsNorTrailsIdenticalPeers: a relay without
// latency measurements, otherwise identical to a pass of identical measured
// relays, scores the same and so heads its fair share of pages.
func TestUnmeasuredRelayNeitherLeadsNorTrailsIdenticalPeers(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var relays []relay.Descriptor
	snapshots := make(map[string]RelayMetricsSnapshot)
	for i := range 4 {
		id := "relay_" + string(rune('a'+i))
		relays = append(relays, relay.Descriptor{ID: id, PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now})
		snapshots[id] = RelayMetricsSnapshot{Successes: 20, TCPMS: 180, InternetProbeMS: 4000}
	}
	relays = append(relays, relay.Descriptor{ID: "relay_new", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now})
	snapshots["relay_new"] = RelayMetricsSnapshot{Successes: 20}

	rankings := rankRelays(relays, RankingInputs{Snapshots: snapshots}, false)
	if got, want := rankings["relay_new"].Score, rankings["relay_a"].Score; math.Abs(got-want) > 1e-12 {
		t.Fatalf("unmeasured relay score = %v, want the measured peers' %v", got, want)
	}
	shares := leadShares(relays, snapshots, nil, 20000)
	if got := shares["relay_new"]; math.Abs(got-0.2) > 0.02 {
		t.Errorf("unmeasured relay leads %.3f of pages, want about 0.2 of five identical relays; shares %v", got, shares)
	}
}

// TestRankRelaysAndSortShareNeutralLatency: the operator breakdown and the
// order clients get must apply the same latency score to an unmeasured relay.
// The measured relays' latency scores are 1, 0.9, 0.7 and 0.3, so the neutral
// value is 0.8 and the unmeasured relay sorts third; a fixed 0.5 would put it
// fourth.
func TestRankRelaysAndSortShareNeutralLatency(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	snapshots := map[string]RelayMetricsSnapshot{"relay_new": {Successes: 10}}
	relays := []relay.Descriptor{{ID: "relay_new", PublicHost: "203.0.113.9", MaxSessions: 100, LastHeartbeatAt: now}}
	for i, score := range []float64{1, 0.9, 0.7, 0.3} {
		id := "relay_" + string(rune('a'+i))
		relays = append(relays, relay.Descriptor{ID: id, PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now})
		snapshots[id] = RelayMetricsSnapshot{Successes: 10, TCPMS: tcpLatencyFor(score)}
	}
	inputs := RankingInputs{Snapshots: snapshots}
	rankings := rankRelays(relays, inputs, false)
	if got := rankings["relay_new"].Latency; math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("unmeasured relay latency = %v, want 0.8", got)
	}

	sortRelayCandidates(relays, snapshots, nil, constantUniform)
	var order []string
	for _, desc := range relays {
		order = append(order, desc.ID)
	}
	if want := "relay_a,relay_b,relay_new,relay_c,relay_d"; strings.Join(order, ",") != want {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := 1; i < len(relays); i++ {
		if rankings[relays[i-1].ID].SelectionWeight <= rankings[relays[i].ID].SelectionWeight {
			t.Fatalf("sort order %v disagrees with rankRelays selection weights %+v", order, rankings)
		}
	}
}

func TestMeasuredHandshakeScoreRampsResentShare(t *testing.T) {
	const minSamples = rankingHandshakeMinSamples
	for _, tc := range []struct {
		name           string
		total, resent  int
		want           float64
		wantUnmeasured bool
	}{
		{name: "no samples", wantUnmeasured: true},
		{name: "below the minimum", total: minSamples - 1, resent: minSamples - 1, wantUnmeasured: true},
		{name: "all fast", total: minSamples, want: 1},
		{name: "mixed", total: 2 * minSamples, resent: 4, want: 0.5},
		{name: "worst share", total: 100, resent: 20, want: 0},
		{name: "all slow", total: minSamples, resent: minSamples, want: 0},
	} {
		snapshot := RelayMetricsSnapshot{TCPHandshakes: tc.total, TCPHandshakesResent: tc.resent}
		got, ok := measuredHandshakeScore(snapshot)
		if ok == tc.wantUnmeasured || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: score = %v (measured %v), want %v (measured %v)", tc.name, got, ok, tc.want, !tc.wantUnmeasured)
		}
		if !tc.wantUnmeasured {
			continue
		}
		// An unmeasured relay takes the pass's neutral value: the default when
		// no relay is measured, otherwise the measured relays' median.
		if got := relayScoreBreakdown(relay.Descriptor{}, snapshot, defaultNeutralScores).Handshake; got != rankingDefaultHandshakeScore {
			t.Errorf("%s: handshake score = %v, want the default %v", tc.name, got, rankingDefaultHandshakeScore)
		}
		relays := []relay.Descriptor{{ID: "relay_new"}, {ID: "relay_a"}, {ID: "relay_b"}, {ID: "relay_c"}}
		snapshots := map[string]RelayMetricsSnapshot{
			"relay_new": snapshot,
			"relay_a":   {TCPHandshakes: 100},
			"relay_b":   {TCPHandshakes: 100, TCPHandshakesResent: 5},
			"relay_c":   {TCPHandshakes: 100, TCPHandshakesResent: 20},
		}
		if got := rankRelays(relays, RankingInputs{Snapshots: snapshots}, false)["relay_new"].Handshake; math.Abs(got-0.75) > 1e-9 {
			t.Errorf("%s: handshake score among measured relays = %v, want their median 0.75", tc.name, got)
		}
	}
}

// TestRelayScoreHandshakeCarriesItsWeight: between otherwise identical relays
// the handshake score moves the total by exactly its ranking weight, and the
// relay with resent handshakes sorts last.
func TestRelayScoreHandshakeCarriesItsWeight(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	relays := []relay.Descriptor{
		{ID: "relay_lossy", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now.Add(time.Second)},
		{ID: "relay_clean", PublicHost: "203.0.113.2", MaxSessions: 100, LastHeartbeatAt: now},
	}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_lossy": {Successes: 40, TCPMS: 180, TCPHandshakes: 40, TCPHandshakesResent: 40},
		"relay_clean": {Successes: 40, TCPMS: 180, TCPHandshakes: 40},
	}
	rankings := rankRelays(relays, RankingInputs{Snapshots: snapshots}, false)
	if got := rankings["relay_clean"].Score - rankings["relay_lossy"].Score; math.Abs(got-rankingHandshakeWeight) > 1e-9 {
		t.Fatalf("score gap = %v, want the handshake weight %v", got, rankingHandshakeWeight)
	}
	sortRelayCandidates(relays, snapshots, nil, constantUniform)
	if relays[0].ID != "relay_clean" {
		t.Fatalf("order = %s, %s; want the relay without resent handshakes first", relays[0].ID, relays[1].ID)
	}
}

// latencyMedianEvents feed a relay TCP samples 100, 200, 5000 and one success
// without a measurement (median 200), internet probe samples 3000 and 4000
// (median 3500, interpolated like percentile_cont). Three handshakes were
// measured, one of them resent.
func latencyMedianEvents(relayID string, at time.Time) []TelemetryRecord {
	samples := []map[string]int64{
		{"relay_tcp_ms": 100, "internet_probe_ms": 3000},
		{"relay_tcp_ms": 5000, "internet_probe_ms": 4000},
		{"relay_tcp_ms": 200},
		{},
	}
	records := make([]TelemetryRecord, len(samples))
	for i, measurements := range samples {
		records[i] = TelemetryRecord{ReceivedAt: at, Event: TelemetryEvent{
			EventID:      "latency-median-" + string(rune('a'+i)),
			Event:        "connection_succeeded",
			OccurredAt:   at,
			RelayID:      relayID,
			Measurements: measurements,
		}}
	}
	return records
}

func assertLatencyMedians(t *testing.T, snapshot RelayMetricsSnapshot) {
	t.Helper()
	if snapshot.Successes != 4 || snapshot.TCPMS != 200 || snapshot.InternetProbeMS != 3500 {
		t.Fatalf("snapshot = %+v, want 4 successes, TCP median 200, probe median 3500", snapshot)
	}
	if snapshot.TCPHandshakes != 3 || snapshot.TCPHandshakesResent != 1 {
		t.Fatalf("snapshot = %+v, want 3 handshakes, 1 resent", snapshot)
	}
}

func TestStoreMetricSnapshotsUseLatencyMedians(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	if err := store.RecordRelayTelemetry(context.Background(), latencyMedianEvents("relay_median", now), now); err != nil {
		t.Fatalf("record telemetry: %v", err)
	}
	store.mu.RLock()
	snapshots, _ := store.metricSnapshotsLocked(now.Add(time.Minute))
	store.mu.RUnlock()
	assertLatencyMedians(t, snapshots["relay_median"])
}
