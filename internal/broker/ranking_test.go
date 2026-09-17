package broker

import (
	"math"
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

	reliableScore := relayScore(desc, reliable, defaultRankingWeight)
	failingScore := relayScore(desc, failing, defaultRankingWeight)
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
		if got := relayScore(desc, snapshot, defaultRankingWeight); math.Abs(got-tc.want) > 1e-9 {
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
	if got := relayScore(desc, snapshot, defaultRankingWeight); got != 0 {
		t.Fatalf("score = %f, want zero floor", got)
	}
}

func TestRelayScoreUnlimitedCapacityHasNoOverloadPenalty(t *testing.T) {
	desc := relay.Descriptor{MaxMbps: 20}
	metrics := RelayMetricsSnapshot{Successes: 8, Failures: 2}
	idleScore := relayScore(desc, metrics, defaultRankingWeight)
	metrics.ActiveSessions = 1000
	if busyScore := relayScore(desc, metrics, defaultRankingWeight); busyScore != idleScore {
		t.Fatalf("unlimited relay score changed with active sessions: idle %f, busy %f", idleScore, busyScore)
	}
}

// TestRelayScoreRankingWeightIsAFinalMultiplier pins the weight's place in the
// formula: it scales the whole telemetry score after the over-capacity penalty
// (so a weighted, overloaded relay is penalized then scaled, never the other
// way round), weight 1 changes nothing, and weight 0 zeroes the score.
func TestRelayScoreRankingWeightIsAFinalMultiplier(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 10, MaxMbps: 100}
	overloaded := RelayMetricsSnapshot{ActiveSessions: 15, Successes: 8, Failures: 2}
	base := relayScore(desc, overloaded, defaultRankingWeight)
	if base <= 0 {
		t.Fatalf("base score must be positive, got %v", base)
	}
	if got := relayScore(desc, overloaded, 1); got != base {
		t.Errorf("weight 1 changed the score: %v vs %v", got, base)
	}
	if got, want := relayScore(desc, overloaded, 0.5), base*0.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("weight 0.5 = %v, want half of %v", got, base)
	}
	if got := relayScore(desc, overloaded, 0); got != 0 {
		t.Errorf("weight 0 = %v, want 0", got)
	}
	// A corrupted backend value cannot inflate a score past the clamp.
	if got := relayScore(desc, overloaded, 4); got != base {
		t.Errorf("weight above 1 = %v, want it clamped to the unweighted %v", got, base)
	}
	// Only direct database corruption can store NaN, but a NaN weight would
	// otherwise reach the inventory's hand-built JSON as an unparseable
	// literal; the resolver maps it to the default instead.
	if got := rankingWeightFor(map[string]float64{"relay_x": math.NaN()}, "relay_x"); got != defaultRankingWeight {
		t.Errorf("NaN weight resolved to %v, want the default", got)
	}
}

// TestSortRelayCandidatesRankingWeightZeroSortsLast: a drained relay stays in
// the list but below every unweighted relay, however weak the latter's
// telemetry, and a partial weight still moves a strong relay below a weaker
// unweighted one once the multiplier outweighs the telemetry gap.
func TestSortRelayCandidatesRankingWeightZeroSortsLast(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	strong := relay.Descriptor{ID: "relay_strong", PublicHost: "203.0.113.1", MaxSessions: 100, LastHeartbeatAt: now}
	weak := relay.Descriptor{ID: "relay_weak", PublicHost: "203.0.113.2", MaxSessions: 100, LastHeartbeatAt: now}
	snapshots := map[string]RelayMetricsSnapshot{
		"relay_strong": {Successes: 50},
		"relay_weak":   {Successes: 1, Failures: 20, ActiveSessions: 100},
	}

	relays := []relay.Descriptor{weak, strong}
	sortRelayCandidates(relays, snapshots, nil)
	if relays[0].ID != "relay_strong" {
		t.Fatalf("unweighted: strong relay should lead, got %q", relays[0].ID)
	}

	relays = []relay.Descriptor{strong, weak}
	sortRelayCandidates(relays, snapshots, map[string]float64{"relay_strong": 0})
	if relays[len(relays)-1].ID != "relay_strong" {
		t.Fatalf("weight 0 must sort the strong relay last, got order %q, %q", relays[0].ID, relays[1].ID)
	}

	relays = []relay.Descriptor{strong, weak}
	sortRelayCandidates(relays, snapshots, map[string]float64{"relay_strong": 0.2})
	if relays[0].ID != "relay_weak" {
		t.Fatalf("weight 0.2 on the strong relay should demote it below the weak one, got %q first", relays[0].ID)
	}
}
