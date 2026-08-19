package broker

import (
	"strings"

	"openrung/internal/relay"
)

// maxDiversitySlots caps how many page slots geographic coverage may
// repurpose, so the global ranking still decides most of the page.
const maxDiversitySlots = 2

// relayRegions maps ISO country codes to the coarse regions used for page
// diversity. It covers every country the fleet's deploy targets span (the
// Hetzner/Linode hosts plus deploy/relay/aws/wss-origin-targets.json), and
// resolveRelayGeo warns when a relay resolves to a code outside the map, so
// the map falling behind the fleet is visible instead of silently shrinking
// coverage. A code outside the map (or an unresolved geo lookup) belongs to
// no region: it can neither claim a diversity slot nor count a region as
// represented, and it is never protected from displacement.
var relayRegions = map[string]string{
	"KR": "asia",
	"JP": "asia",
	"SG": "asia",
	"IN": "asia",
	"HK": "asia",
	"ID": "asia",
	"DE": "europe",
	"FI": "europe",
	"SE": "europe",
	"US": "americas",
	"BR": "americas",
	"AU": "oceania",
}

func regionForCountryCode(code string) string {
	return relayRegions[strings.ToUpper(code)]
}

func relayRegion(desc relay.Descriptor) string {
	return regionForCountryCode(desc.CountryCode)
}

// diversifyRelayPage rewrites the head of the globally ranked relay list —
// the served page — so every region the eligible fleet spans is represented
// on it: each region missing from the page swaps its best-ranked below-fold
// relay with a relay from the page's tail. The page is exactly the probe set
// of the clients' latency ranker, so carrying at least one relay per region
// lets every client steer itself to its nearest region without the broker
// knowing where the client is. The global #1 is never displaced, at most
// maxDiversitySlots relays are, and displaced relays swap below the fold
// rather than disappearing, so the WSS reservation that runs after this can
// still consider them.
func diversifyRelayPage(relays []relay.Descriptor, limit int) []relay.Descriptor {
	if limit <= 1 || len(relays) <= limit {
		return relays
	}

	// representatives counts each known region's relays on the page so a fill
	// never displaces a region's only representative: coverage must only ever
	// grow, or the pass could trade one missing region for another.
	representatives := make(map[string]int)
	pageHasWSS := false
	for _, desc := range relays[:limit] {
		if region := relayRegion(desc); region != "" {
			representatives[region]++
		}
		pageHasWSS = pageHasWSS || wssRelayEligible(desc)
	}

	// Global order below the fold picks each missing region's best relay.
	var fills []int
	fillHasWSS := false
	claimed := make(map[string]bool)
	for i := limit; i < len(relays) && len(fills) < maxDiversitySlots; i++ {
		region := relayRegion(relays[i])
		if region == "" || representatives[region] > 0 || claimed[region] {
			continue
		}
		claimed[region] = true
		fills = append(fills, i)
		fillHasWSS = fillHasWSS || wssRelayEligible(relays[i])
	}
	if len(fills) == 0 {
		return relays
	}

	// reserveWSSCandidate overwrites the last slot when the page carries no
	// WSS-eligible relay and one exists below the fold. When no fill would
	// satisfy the reservation itself, keep fills out of that slot, or the
	// reservation would silently cancel a fill.
	slot := limit - 1
	if !pageHasWSS && !fillHasWSS {
		for _, desc := range relays[limit:] {
			if wssRelayEligible(desc) {
				slot = limit - 2
				break
			}
		}
	}

	out := append([]relay.Descriptor(nil), relays...)
	for _, fill := range fills {
		// The victim is the deepest page slot that is neither the global #1
		// nor its region's only representative on the page.
		for slot >= 1 {
			region := relayRegion(out[slot])
			if region == "" || representatives[region] > 1 {
				break
			}
			slot--
		}
		if slot < 1 {
			break
		}
		if region := relayRegion(out[slot]); region != "" {
			representatives[region]--
		}
		representatives[relayRegion(out[fill])]++
		out[slot], out[fill] = out[fill], out[slot]
		slot--
	}
	return out
}
