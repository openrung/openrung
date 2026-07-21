package broker

import (
	"math"
	"testing"

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
