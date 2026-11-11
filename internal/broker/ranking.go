package broker

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"openrung/internal/relay"
)

const (
	rankingWindow = 30 * time.Minute

	// Reliability is the strongest ranking signal: spare capacity is useful only
	// when clients can actually reach the relay. Keeping headroom below the
	// reliability weight also prevents an idle, repeatedly failing relay from
	// displacing a busy relay with a strong recent success record.
	rankingReliabilityWeight = 0.50
	rankingHeadroomWeight    = 0.20
	rankingLatencyWeight     = 0.20
	rankingHandshakeWeight   = 0.10

	// A full relay has already forfeited the entire 0.20 headroom contribution.
	// Continue applying back-pressure above capacity, but cap it below the 0.05
	// margin by which a 75%-reliable full relay outscores a 25%-reliable idle
	// one when their latency and handshake signals are equal.
	rankingMaxOverCapacityPenalty = 0.04

	// defaultRankingWeight is the operator multiplier every relay carries until
	// an override is stored: the telemetry-driven score is used as-is.
	defaultRankingWeight = 1.0

	// rankingSelectionSharpness is the exponent applied to a relay's telemetry
	// score before it becomes a sampling weight (see sortRelayCandidates). It
	// sets how strongly a score gap turns into a lead: a relay scoring 10%
	// below another is drawn ahead of it about one time in five, and one
	// scoring a third lower less than one time in a hundred.
	rankingSelectionSharpness = 12

	// Each latency measurement is scored from its median over rankingWindow:
	// 1 at or below its fast bound, falling linearly to 0 at its slow bound.
	// TCP connect is one handshake with the relay.
	rankingTCPFastMS = 100
	rankingTCPSlowMS = 1000
	// The internet probe makes a full round trip through the tunnel, so its
	// ramp is ten times as long.
	rankingTunnelRoundTripFastMS = 1000
	rankingTunnelRoundTripSlowMS = 10000

	// rankingDefaultLatencyScore is the latency score of a relay without
	// latency measurements when no relay in the ranking pass has any either.
	// Otherwise such a relay gets the pass's median (see neutralScores).
	rankingDefaultLatencyScore = 0.5

	// A TCP handshake that needs no retransmission takes one round trip. RFC
	// 6298 section 2.1 starts the retransmission timer at one second, before
	// any round-trip time has been measured, so a handshake that took a second
	// or longer had its SYN or SYN-ACK resent, or a round trip that long.
	rankingHandshakeResentMS = 1000

	// The handshake score is 1 when no handshake in rankingWindow was resent,
	// falling linearly to 0 at this share of them. A handshake is resent when
	// either of its first two segments is lost, so while loss is low the share
	// is about twice the path's packet loss rate. One handshake in five is
	// therefore about one packet in ten, where the retransmission-timeout term
	// of the TCP throughput equation (RFC 5348 section 3.1) is the larger one.
	rankingHandshakeResentWorstShare = 0.20

	// rankingHandshakeMinSamples is the fewest handshakes in rankingWindow the
	// share is scored from: one resent handshake among this many moves the
	// share by a quarter of the ramp. A relay with fewer is unmeasured.
	rankingHandshakeMinSamples = 20

	// rankingDefaultHandshakeScore is the handshake score of an unmeasured
	// relay when no relay in the ranking pass is measured either. Otherwise
	// such a relay gets the pass's median (see neutralScores).
	rankingDefaultHandshakeScore = 0.5
)

// rankingUniform is the [0, 1) source the stores draw ranking keys from. It is
// a variable only so tests can pin the order; math/rand/v2's top-level source
// is safe for concurrent use.
var rankingUniform = rand.Float64

type RelayMetricsSnapshot struct {
	ActiveSessions int
	Successes      int
	Failures       int
	// Median latencies over rankingWindow in milliseconds; 0 means the relay
	// has no positive measurement of that kind in the window.
	TCPMS           float64
	InternetProbeMS float64
	// TCP handshakes measured in rankingWindow, and how many of them took
	// rankingHandshakeResentMS or longer.
	TCPHandshakes       int
	TCPHandshakesResent int
}

type relaySessionState struct {
	ClientID          string
	RelayID           string
	LastHeartbeatAt   time.Time
	TerminalAt        time.Time
	LastMetricEventAt time.Time
}

type relayMetricObservation struct {
	ObservedAt      time.Time
	RelayID         string
	Success         bool
	Failure         bool
	TCPMs           int64
	InternetProbeMs int64
}

// sortRelayCandidates orders relays best-first by weighted random sampling
// without replacement: each relay is drawn ahead of the rest with probability
// proportional to its selection weight (relaySelectionWeight). A deterministic
// best-score-first order would hand every client the same head of the page,
// so two relays with near-equal scores would trade the whole load back and
// forth as their telemetry crosses; sampling splits it between them instead,
// while a clearly better relay still leads almost every page.
//
// snapshots is the 30-minute telemetry view per relay ID; weights is the
// operator's per-relay ranking multiplier keyed the same way, with absent
// entries meaning defaultRankingWeight (see rankingWeightFor). uniform returns
// values in [0, 1); a constant source strictly inside (0, 1) reproduces the
// plain best-selection-weight-first order. Relays with selection weight 0
// (operator weight 0, or a telemetry score of 0) always sort after every
// positive-weight relay.
//
// relays must be the whole ranking pass: it sets the latency and handshake
// scores of unmeasured relays (see neutralScores), and rankRelays derives the
// same values from the same set.
func sortRelayCandidates(relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot, weights map[string]float64, uniform func() float64) {
	neutral := neutralScores(relays, snapshots)
	// Efraimidis–Spirakis keys: sorting by U^(1/w) descending is weighted
	// sampling without replacement. The log form, ln(U)/w, orders identically
	// and stays finite for tiny weights.
	keys := make(map[string]float64, len(relays))
	for _, desc := range relays {
		weight := relaySelectionWeight(relayScore(desc, snapshots[desc.ID], neutral), rankingWeightFor(weights, desc.ID))
		if weight <= 0 {
			keys[desc.ID] = math.Inf(-1)
			continue
		}
		// 1-U lies in (0, 1], so the logarithm is finite and at most 0.
		keys[desc.ID] = math.Log1p(-uniform()) / weight
	}

	sort.SliceStable(relays, func(i, j int) bool {
		left, right := relays[i], relays[j]
		leftKey, rightKey := keys[left.ID], keys[right.ID]
		if leftKey != rightKey {
			return leftKey > rightKey
		}
		if !left.LastHeartbeatAt.Equal(right.LastHeartbeatAt) {
			return left.LastHeartbeatAt.After(right.LastHeartbeatAt)
		}
		leftIPv6 := relay.IsIPv6Host(left.PublicHost)
		rightIPv6 := relay.IsIPv6Host(right.PublicHost)
		if leftIPv6 != rightIPv6 {
			return leftIPv6
		}
		return left.ID < right.ID
	})
}

// relaySelectionWeight turns a relay's telemetry score and operator weight
// into its sampling weight: score^rankingSelectionSharpness × weight. The
// exponent applies to telemetry only, so the operator weight stays a linear
// dial on a relay's share of page heads among its peers: weight 1 leaves the
// telemetry ranking untouched, 0.5 halves the relay's odds of being drawn
// ahead of an otherwise equal relay, and 0 pins it to the bottom of the list
// without delisting it.
func relaySelectionWeight(score, weight float64) float64 {
	return math.Pow(clamp01(score), rankingSelectionSharpness) * clamp01(weight)
}

// rankingWeightFor resolves a relay's effective operator weight: the stored
// override when one exists, otherwise defaultRankingWeight. Stored values are
// already validated into [0, 1] by the store's setter, so an out-of-range entry
// can only mean a corrupted backend; clamp rather than let it inflate a score,
// and treat NaN (which clamp01 would pass through, and which the inventory's
// hand-built JSON would then emit as an unparseable literal) as the default.
func rankingWeightFor(weights map[string]float64, relayID string) float64 {
	weight, ok := weights[relayID]
	if !ok || math.IsNaN(weight) {
		return defaultRankingWeight
	}
	return clamp01(weight)
}

// relayScore is the telemetry-driven candidate score in [0, 1]. The operator's
// ranking weight is applied separately, in relaySelectionWeight. neutral is
// the ranking pass's neutralScores.
func relayScore(desc relay.Descriptor, snapshot RelayMetricsSnapshot, neutral rankingNeutralScores) float64 {
	return relayScoreBreakdown(desc, snapshot, neutral).score()
}

// relayScoreParts are relayScore's inputs, each in [0, 1] before its ranking
// weight is applied, plus the over-capacity penalty subtracted at the end.
type relayScoreParts struct {
	Reliability         float64
	Headroom            float64
	Latency             float64
	Handshake           float64
	OverCapacityPenalty float64
}

func relayScoreBreakdown(desc relay.Descriptor, snapshot RelayMetricsSnapshot, neutral rankingNeutralScores) relayScoreParts {
	headroom := 0.5
	if desc.MaxSessions > 0 {
		headroom = clamp01(float64(desc.MaxSessions-snapshot.ActiveSessions) / float64(desc.MaxSessions))
	}
	return relayScoreParts{
		Reliability:         float64(snapshot.Successes+1) / float64(snapshot.Successes+snapshot.Failures+2),
		Headroom:            headroom,
		Latency:             observedScore(measuredLatencyScore, snapshot, neutral.Latency),
		Handshake:           observedScore(measuredHandshakeScore, snapshot, neutral.Handshake),
		OverCapacityPenalty: overCapacityPenalty(desc, snapshot),
	}
}

func (p relayScoreParts) score() float64 {
	// Headroom declines continuously to zero at capacity. Beyond capacity, keep
	// a small gradient rather than the old threshold multiplier: the bounded
	// penalty supplies back-pressure without letting load dominate reliability.
	score := rankingReliabilityWeight*p.Reliability +
		rankingHeadroomWeight*p.Headroom +
		rankingLatencyWeight*p.Latency +
		rankingHandshakeWeight*p.Handshake
	return clamp01(score - p.OverCapacityPenalty)
}

// RankingInputs is what ListRanked sorted with: the 30-minute telemetry
// snapshot per relay ID (absent for a relay without telemetry) and the
// operator ranking-weight overrides. Page-shaping steps after the sort and the
// operator views read them so they agree with the order without a second read.
type RankingInputs struct {
	Weights   map[string]float64
	Snapshots map[string]RelayMetricsSnapshot
}

// relayRanking is the operator view of one relay's place in the ranking: the
// telemetry score and its parts, the selection weight the sampler draws with,
// and HeadShare, the chance the relay heads a public relay-list response.
type relayRanking struct {
	Score               float64 `json:"score"`
	Reliability         float64 `json:"reliability"`
	Headroom            float64 `json:"headroom"`
	Latency             float64 `json:"latency"`
	Handshake           float64 `json:"handshake"`
	OverCapacityPenalty float64 `json:"over_capacity_penalty"`
	SelectionWeight     float64 `json:"selection_weight"`
	HeadShare           float64 `json:"head_share"`
}

// rankRelays computes relayRanking for every relay in relays. Under weighted
// sampling without replacement a relay is drawn first with probability equal
// to its share of the total selection weight, so HeadShare is that share among
// the relays the public list would carry: when withholdUnreachable (probing
// enabled, the public handler's own policy) a relay withheld by a failed
// reachability probe gets 0 and is left out of the total. HeadShare ignores
// the WSS reservation and the access answer's per-country candidate sets, so
// it describes the public list, not every page.
//
// relays must be the same set ListRanked sorted, so the latency and handshake
// scores of unmeasured relays match the ones the sort used.
func rankRelays(relays []relay.Descriptor, inputs RankingInputs, withholdUnreachable bool) map[string]relayRanking {
	rankings := make(map[string]relayRanking, len(relays))
	neutral := neutralScores(relays, inputs.Snapshots)
	var total float64
	for _, desc := range relays {
		parts := relayScoreBreakdown(desc, inputs.Snapshots[desc.ID], neutral)
		score := parts.score()
		ranking := relayRanking{
			Score:               score,
			Reliability:         parts.Reliability,
			Headroom:            parts.Headroom,
			Latency:             parts.Latency,
			Handshake:           parts.Handshake,
			OverCapacityPenalty: parts.OverCapacityPenalty,
			SelectionWeight:     relaySelectionWeight(score, rankingWeightFor(inputs.Weights, desc.ID)),
		}
		if !withholdUnreachable || !relayUnreachable(desc) {
			total += ranking.SelectionWeight
		}
		rankings[desc.ID] = ranking
	}
	if total > 0 {
		for _, desc := range relays {
			if withholdUnreachable && relayUnreachable(desc) {
				continue
			}
			ranking := rankings[desc.ID]
			ranking.HeadShare = ranking.SelectionWeight / total
			rankings[desc.ID] = ranking
		}
	}
	return rankings
}

func overCapacityPenalty(desc relay.Descriptor, snapshot RelayMetricsSnapshot) float64 {
	if desc.MaxSessions <= 0 || snapshot.ActiveSessions <= desc.MaxSessions {
		return 0
	}
	overCapacityRatio := float64(snapshot.ActiveSessions-desc.MaxSessions) / float64(desc.MaxSessions)
	return rankingMaxOverCapacityPenalty * clamp01(overCapacityRatio)
}

// observedScore is the relay's measured score, or neutral when it is
// unmeasured.
func observedScore(measured measuredScoreFunc, snapshot RelayMetricsSnapshot, neutral float64) float64 {
	if score, ok := measured(snapshot); ok {
		return score
	}
	return neutral
}

// measuredScoreFunc scores one telemetry signal of a relay; ok is false when
// the relay's snapshot does not measure it.
type measuredScoreFunc func(RelayMetricsSnapshot) (score float64, ok bool)

// measuredLatencyScore averages the scores of the latency measurements the
// relay has, each scored from its median on its own ramp. ok is false when it
// has none.
func measuredLatencyScore(snapshot RelayMetricsSnapshot) (score float64, ok bool) {
	measurements := []struct{ medianMS, fastMS, slowMS float64 }{
		{snapshot.TCPMS, rankingTCPFastMS, rankingTCPSlowMS},
		{snapshot.InternetProbeMS, rankingTunnelRoundTripFastMS, rankingTunnelRoundTripSlowMS},
	}
	var total float64
	var count int
	for _, m := range measurements {
		if m.medianMS <= 0 {
			continue
		}
		total += 1 - clamp01((m.medianMS-m.fastMS)/(m.slowMS-m.fastMS))
		count++
	}
	if count == 0 {
		return 0, false
	}
	return total / float64(count), true
}

// measuredHandshakeScore scores the share of the relay's TCP handshakes that
// were resent: 1 for none, falling linearly to 0 at
// rankingHandshakeResentWorstShare. ok is false below
// rankingHandshakeMinSamples handshakes.
func measuredHandshakeScore(snapshot RelayMetricsSnapshot) (score float64, ok bool) {
	if snapshot.TCPHandshakes < rankingHandshakeMinSamples {
		return 0, false
	}
	resentShare := float64(snapshot.TCPHandshakesResent) / float64(snapshot.TCPHandshakes)
	return 1 - clamp01(resentShare/rankingHandshakeResentWorstShare), true
}

// rankingNeutralScores are the scores a ranking pass gives its unmeasured
// relays, one per signal that can be missing.
type rankingNeutralScores struct {
	Latency   float64
	Handshake float64
}

// neutralScores is the pass's rankingNeutralScores: per signal, the median
// score of the relays in the pass that measure it, or the signal's default
// when none does. A fixed middle value would sit below a typical measured
// relay and keep a new relay off page heads before it could collect any
// measurements.
func neutralScores(relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot) rankingNeutralScores {
	return rankingNeutralScores{
		Latency:   medianMeasuredScore(measuredLatencyScore, relays, snapshots, rankingDefaultLatencyScore),
		Handshake: medianMeasuredScore(measuredHandshakeScore, relays, snapshots, rankingDefaultHandshakeScore),
	}
}

func medianMeasuredScore(measured measuredScoreFunc, relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot, fallback float64) float64 {
	var scores []float64
	for _, desc := range relays {
		if score, ok := measured(snapshots[desc.ID]); ok {
			scores = append(scores, score)
		}
	}
	if len(scores) == 0 {
		return fallback
	}
	return median(scores)
}

// median returns the middle of values, averaging the two middle values of an
// even count as Postgres's percentile_cont(0.5) does, or 0 for no values. It
// sorts values in place.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return (values[mid-1] + values[mid]) / 2
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
