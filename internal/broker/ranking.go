package broker

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"openrung/internal/relay"
)

const rankingWindow = 30 * time.Minute

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

func sortRelayCandidates(relays []relay.Descriptor, snapshots map[string]RelayMetricsSnapshot, mode RankingMode) {
	if mode == RankingModeLegacy {
		sortLegacyRelays(relays)
		return
	}

	scores := make(map[string]float64, len(relays))
	for _, desc := range relays {
		scores[desc.ID] = relayScore(desc, snapshots[desc.ID])
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

func relayScore(desc relay.Descriptor, snapshot RelayMetricsSnapshot) float64 {
	headroom := 0.5
	if desc.MaxSessions > 0 {
		headroom = clamp01(float64(desc.MaxSessions-snapshot.ActiveSessions) / float64(desc.MaxSessions))
	}

	successRate := float64(snapshot.Successes+1) / float64(snapshot.Successes+snapshot.Failures+2)
	latencyScore := observedLatencyScore(snapshot)
	speedScore := observedSpeedScore(desc, snapshot)

	score := 0.45*headroom + 0.25*successRate + 0.20*latencyScore + 0.10*speedScore
	if desc.MaxSessions > 0 && snapshot.ActiveSessions >= desc.MaxSessions {
		score *= 0.2
	}
	return score
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
