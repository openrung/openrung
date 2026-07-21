package broker

import (
	"testing"

	"openrung/internal/relay"
)

func TestRelayScoreReliabilityDominatesHeadroom(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}

	// Force reliability and capacity into direct conflict: the reliable relay is
	// full while the failing relay is idle. Reliability must win or a small
	// default page excludes the relay that clients can actually use.
	reliable := RelayMetricsSnapshot{
		ActiveSessions: 8,
		Successes:      8,
		Failures:       2,
	}
	failing := RelayMetricsSnapshot{Successes: 2, Failures: 8}

	reliableScore := relayScore(desc, reliable)
	failingScore := relayScore(desc, failing)
	if reliableScore <= failingScore {
		t.Fatalf("reliable full relay score %f must exceed idle failing relay score %f", reliableScore, failingScore)
	}
}

func TestRelayScoreHasNoOverCapacityCliff(t *testing.T) {
	desc := relay.Descriptor{MaxSessions: 8, MaxMbps: 20}
	metrics := RelayMetricsSnapshot{Successes: 8, Failures: 2}
	justBelow := metrics
	justBelow.ActiveSessions = 7
	atCapacity := metrics
	atCapacity.ActiveSessions = 8
	slightlyOver := metrics
	slightlyOver.ActiveSessions = 9

	justBelowScore := relayScore(desc, justBelow)
	atCapacityScore := relayScore(desc, atCapacity)
	slightlyOverScore := relayScore(desc, slightlyOver)
	if atCapacityScore >= justBelowScore {
		t.Fatalf("capacity headroom did not lower score: below %f, at capacity %f", justBelowScore, atCapacityScore)
	}
	if atCapacityScore <= justBelowScore*0.90 {
		t.Fatalf("reaching capacity caused a score cliff: score fell from %f to %f", justBelowScore, atCapacityScore)
	}
	if slightlyOverScore != atCapacityScore {
		t.Fatalf("one extra session added a second capacity penalty: score changed from %f to %f", atCapacityScore, slightlyOverScore)
	}
}
