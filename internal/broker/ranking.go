package broker

import (
	"errors"
	"math"
	"sort"
	"strings"
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
	rankingSpeedWeight       = 0.10

	// A full relay has already forfeited the entire 0.20 headroom contribution.
	// Continue applying back-pressure above capacity, but cap it below the 0.05
	// margin by which a 75%-reliable full relay beats a 25%-reliable idle one
	// when their latency and speed signals are equal.
	rankingMaxOverCapacityPenalty = 0.04

	// defaultRankingWeight is the operator multiplier every relay carries until
	// an override is stored: the telemetry-driven score is used as-is.
	defaultRankingWeight = 1.0
)

type metricValue struct {
	total float64
	count int
}

type RelayMetricsSnapshot struct {
	ActiveSessions    int
	Successes         int
	Failures          int
	TCPMS             metricValue
	TunnelStartMS     metricValue
	InternetProbeMS   metricValue
	TTFBMS            metricValue
	SpeedTests        int
	DownloadMbpsTotal float64
}

type relaySessionState struct {
	ClientID          string
	RelayID           string
	LastHeartbeatAt   time.Time
	TerminalAt        time.Time
	LastMetricEventAt time.Time
}

type relayMetricObservation struct {
	ObservedAt        time.Time
	RelayID           string
	Success           bool
	Failure           bool
	TCPMs             int64
	TunnelStartMs     int64
	InternetProbeMs   int64
	TTFBMs            int64
	DownloadMbpsMilli int64
	IncludesSpeedTest bool
}

func normalizeRankingMode(mode RankingMode) RankingMode {
	switch RankingMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case RankingModeLegacy:
		return RankingModeLegacy
	default:
		return RankingModeGlobal
	}
}

func ParseRankingMode(raw string) (RankingMode, error) {
	switch RankingMode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", RankingModeGlobal:
		return RankingModeGlobal, nil
	case RankingModeLegacy:
		return RankingModeLegacy, nil
	default:
		return "", errors.New("relay-ranking must be global or legacy")
	}
}

// sortRelayCandidates orders relays best-first. snapshots is the 30-minute
// telemetry view per relay ID; weights is the operator's per-relay ranking
// multiplier keyed the same way, with absent entries meaning
// defaultRankingWeight (see rankingWeightFor). Legacy mode ignores both.
func sortRelayCandidates(relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot, weights map[string]float64, mode RankingMode) {
	if mode == RankingModeLegacy {
		sortLegacyRelays(relays)
		return
	}

	scores := make(map[string]float64, len(relays))
	for _, desc := range relays {
		scores[desc.ID] = relayScore(desc, snapshots[desc.ID], rankingWeightFor(weights, desc.ID))
	}

	sort.SliceStable(relays, func(i, j int) bool {
		left, right := relays[i], relays[j]
		leftScore, rightScore := scores[left.ID], scores[right.ID]
		if math.Abs(leftScore-rightScore) > 0.000001 {
			return leftScore > rightScore
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

func sortLegacyRelays(relays []relay.Descriptor) {
	sort.Slice(relays, func(i, j int) bool {
		iIPv6 := relay.IsIPv6Host(relays[i].PublicHost)
		jIPv6 := relay.IsIPv6Host(relays[j].PublicHost)
		if iIPv6 != jIPv6 {
			return iIPv6
		}
		return relays[i].LastHeartbeatAt.After(relays[j].LastHeartbeatAt)
	})
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

// relayScore is the telemetry-driven candidate score in [0, 1], scaled last by
// the operator's ranking weight. The weight multiplies the whole score (after
// the over-capacity penalty, before the clamp) rather than adding a term, so
// it is a pure dial on top of the telemetry signals: weight 1 leaves the
// ranking untouched, weight 0 pins the relay to the bottom of the list without
// delisting it, and any value between shifts load away in proportion.
func relayScore(desc relay.Descriptor, snapshot RelayMetricsSnapshot, weight float64) float64 {
	headroom := 0.5
	if desc.MaxSessions > 0 {
		headroom = clamp01(float64(desc.MaxSessions-snapshot.ActiveSessions) / float64(desc.MaxSessions))
	}

	successRate := float64(snapshot.Successes+1) / float64(snapshot.Successes+snapshot.Failures+2)
	latencyScore := observedLatencyScore(snapshot)
	speedScore := observedSpeedScore(desc, snapshot)

	// Headroom declines continuously to zero at capacity. Beyond capacity, keep
	// a small gradient rather than the old threshold multiplier: the bounded
	// penalty supplies back-pressure without letting load dominate reliability.
	score := rankingReliabilityWeight*successRate +
		rankingHeadroomWeight*headroom +
		rankingLatencyWeight*latencyScore +
		rankingSpeedWeight*speedScore
	return clamp01((score - overCapacityPenalty(desc, snapshot)) * clamp01(weight))
}

func overCapacityPenalty(desc relay.Descriptor, snapshot RelayMetricsSnapshot) float64 {
	if desc.MaxSessions <= 0 || snapshot.ActiveSessions <= desc.MaxSessions {
		return 0
	}
	overCapacityRatio := float64(snapshot.ActiveSessions-desc.MaxSessions) / float64(desc.MaxSessions)
	return rankingMaxOverCapacityPenalty * clamp01(overCapacityRatio)
}

func observedLatencyScore(snapshot RelayMetricsSnapshot) float64 {
	latencies := []float64{
		snapshot.TCPMS.average(),
		snapshot.InternetProbeMS.average(),
		snapshot.TTFBMS.average(),
	}
	var total float64
	var count int
	for _, value := range latencies {
		if value <= 0 {
			continue
		}
		total += value
		count++
	}
	if count == 0 {
		return 0.5
	}
	average := total / float64(count)
	return 1 - clamp01((average-100)/1900)
}

func observedSpeedScore(desc relay.Descriptor, snapshot RelayMetricsSnapshot) float64 {
	if snapshot.SpeedTests == 0 || desc.MaxMbps <= 0 {
		return 0.5
	}
	averageMbps := snapshot.DownloadMbpsTotal / float64(snapshot.SpeedTests)
	return clamp01(averageMbps / float64(desc.MaxMbps))
}

func addMetricValue(value *metricValue, measured int64) {
	if measured <= 0 {
		return
	}
	value.total += float64(measured)
	value.count++
}

func (v metricValue) average() float64 {
	if v.count == 0 {
		return 0
	}
	return v.total / float64(v.count)
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
