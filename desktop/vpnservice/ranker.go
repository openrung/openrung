package vpnservice

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"openrung/desktop/config"
	"openrung/internal/relay"
)

// Client-side latency ranking for the relay connect ladder. Port of the mobile
// RelayRanker (Android net/RelayRanker.kt, iOS Shared/RelayRanker.swift).
//
// The broker already orders relays by a composite score (success rate, load
// headroom, latency, speed) from its own vantage; the one signal it cannot know is
// THIS client's network path. The ranker probes TCP connect latency to the head
// of the candidate list in parallel and reorders by latency BUCKET, so broker
// order — and with it the broker's load balancing — still decides among relays
// whose measured latency falls in the same bucket.
//
// Bucketing is what makes the reorder safe to ship: the broker weights recent
// reliability most heavily but still uses load headroom, and it has no admission
// control, so position in the list helps keep clients off a saturated relay.
// Sorting on raw latency would hand the ladder to the nearest relay for a 2ms win
// and herd every client in a region onto it. Note the buckets are absolute (probeMs /
// RelayRankBucketMS, truncating), not relative clusters: 29ms and 31ms fall in
// different buckets while 31ms and 59ms share one. The compromise is coarse, not
// exact — it bounds how often the client overrides the broker, rather than
// overriding it only for differences a user could feel.
//
// Ranking is fail-open by design: it reorders candidates but never drops one. A
// failed or timed-out probe sinks that relay below the reachable ones (the
// ladder's own RelayTCPTimeout gate may still connect where the shorter probe
// gave up), and candidates beyond RelayRankMaxProbes keep broker order after the
// probed head. If every probe fails, the output is the broker's order unchanged.
//
// Caveat: the probe targets PublicHost, which is the exit only for relays with
// transport "direct". IsUsableRelay gates on ExitMode, not Transport, so a
// tunnel-transport relay can be a candidate today — and for one, PublicHost is
// the relay hub, so its probe measures the path to the hub rather than to the
// exit. The ladder's own relay_tcp_ms already dials the same endpoint, so
// ranking acts on that existing inaccuracy rather than introducing a new one.

// rankedRelay is a candidate plus what the ranker measured for it. probeMS is
// nil when the relay was not probed (pinned connect, single candidate, or past
// the probed head) or when its probe failed — which is distinct from a probe
// that legitimately measured 0ms, since relayTCPReachable floors to whole
// milliseconds and a nearby relay really can come back as 0.
type rankedRelay struct {
	relay   relay.Descriptor
	probeMS *int64
}

// unranked marks every candidate as unprobed, preserving broker order. It is the
// shape rankByTCPLatency returns when there is nothing to rank.
func unranked(cands []relay.Descriptor) []rankedRelay {
	out := make([]rankedRelay, 0, len(cands))
	for _, cand := range cands {
		out = append(out, rankedRelay{relay: cand})
	}
	return out
}

// rankByTCPLatency probes the head of cands in parallel and returns every
// candidate — reachable ones first, ordered by latency bucket with broker order
// preserved inside a bucket, then the probed-but-failed ones, then the unprobed
// tail. Each probe is bounded by probeTimeout; ctx cancels the whole fan-out.
//
// The caller decides whether to rank at all (see Service.rankLadder): a pinned
// relay is never reordered.
func rankByTCPLatency(
	ctx context.Context,
	cands []relay.Descriptor,
	maxProbes int,
	probeTimeout time.Duration,
	bucketMS int64,
	probe func(context.Context, string, int) (int64, error),
) []rankedRelay {
	// Nothing to reorder: skip the probes entirely rather than open sockets whose
	// result cannot change the ladder.
	if len(cands) < 2 {
		return unranked(cands)
	}
	if bucketMS < 1 {
		bucketMS = 1 // every latency its own bucket; never divide by zero
	}

	head, tail := cands, []relay.Descriptor(nil)
	if len(cands) > maxProbes {
		head, tail = cands[:maxProbes], cands[maxProbes:]
	}

	// Probe the head concurrently: the ladder is a user-visible wait, so the cost
	// of ranking must be one probe timeout, not the sum of them.
	probed := make([]rankedRelay, len(head))
	var wg sync.WaitGroup
	for i, cand := range head {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probed[i] = rankedRelay{relay: cand}
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			// A failure, a timeout, and a racing disconnect all land here as a nil
			// probeMS. That is fine: the relay keeps its place in the ladder, and a
			// disconnect is caught by the ctx check runLadder does before its first
			// rung — the ranker never decides a relay is unusable.
			ms, err := probe(probeCtx, cand.PublicHost, cand.PublicPort)
			if err != nil {
				return
			}
			probed[i].probeMS = &ms
		}()
	}
	wg.Wait()

	// Sort the reachable relays on (bucket, broker index) rather than leaning on
	// sort stability: the explicit index tiebreak is a total order, so it holds
	// regardless of which sort a future refactor reaches for.
	order := make([]int, 0, len(probed))
	failed := make([]rankedRelay, 0, len(probed))
	for i, p := range probed {
		if p.probeMS == nil {
			failed = append(failed, p)
			continue
		}
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		lhs, rhs := order[a], order[b]
		lhsBucket := *probed[lhs].probeMS / bucketMS
		rhsBucket := *probed[rhs].probeMS / bucketMS
		if lhsBucket != rhsBucket {
			return lhsBucket < rhsBucket
		}
		return lhs < rhs
	})

	out := make([]rankedRelay, 0, len(cands))
	for _, i := range order {
		out = append(out, probed[i])
	}
	out = append(out, failed...)
	return append(out, unranked(tail)...)
}

// rankLadder reorders (never shrinks) the ladder by this client's measured TCP
// latency, returning the order to walk plus the view the winner's telemetry
// needs. It is fail-open: it reports no error and no failure stage, because a
// ranking that goes wrong must still leave a usable ladder. A racing disconnect
// is caught by the ctx check runLadder does before its first rung.
//
// A pinned relay skips ranking: the user chose it, so there is nothing to
// reorder. A country-targeted connect ranks within the filtered set.
func (s *Service) rankLadder(ctx context.Context, cands []relay.Descriptor, targetRelayID string) ladderOrder {
	order := ladderOrder{brokerOrder: cands}
	if strings.TrimSpace(targetRelayID) != "" || len(cands) < 2 {
		order.ranked = unranked(cands)
		return order
	}
	probes := len(cands)
	if probes > config.RelayRankMaxProbes {
		probes = config.RelayRankMaxProbes
	}
	s.appendLog(fmt.Sprintf("measuring TCP latency to %d relays", probes))
	order.ranked = rankByTCPLatency(
		ctx,
		cands,
		config.RelayRankMaxProbes,
		config.RelayRankProbeTimeout,
		config.RelayRankBucketMS,
		s.relayDialer(),
	)
	return order
}

// ladderOrder is the candidate list the ladder will walk plus the ranking view
// that produced it: the broker's order before ranking, and what each probed
// relay measured. It exists to answer the two questions the winner's telemetry
// asks — where did this relay sit before the client reordered, and what did the
// client measure for it.
type ladderOrder struct {
	brokerOrder []relay.Descriptor // post-filter, pre-ranking, as the broker served it
	ranked      []rankedRelay      // ladder order; probeMS nil when unprobed
}

// candidates is the ladder order as plain descriptors.
func (o ladderOrder) candidates() []relay.Descriptor {
	out := make([]relay.Descriptor, 0, len(o.ranked))
	for _, r := range o.ranked {
		out = append(out, r.relay)
	}
	return out
}

// annotate stamps the winning candidate with its rank observability, so
// connection_succeeded / relay_failover can report whether client ranking
// actually beat broker order.
func (o ladderOrder) annotate(res *candidateResult) {
	if res == nil {
		return
	}
	res.brokerIndex = -1
	for i, cand := range o.brokerOrder {
		if cand.ID == res.relay.ID {
			res.brokerIndex = int64(i)
			break
		}
	}
	for _, r := range o.ranked {
		if r.relay.ID == res.relay.ID {
			res.rankProbeMS = r.probeMS
			return
		}
	}
}
