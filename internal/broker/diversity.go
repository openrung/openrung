package broker

import (
	"strings"

	"openrung/internal/relay"
)

// maxDiversitySlots caps how many page slots geographic coverage may
// repurpose, so the global ranking still decides most of the page.
const maxDiversitySlots = 2

// relayRegions maps ISO country codes to the coarse regions used for page
// diversity. Deliberately tiny and fleet-shaped: a code outside the map (or a
// descriptor whose geo lookup has not resolved) belongs to no region, so it
// can neither claim a diversity slot nor count a region as represented.
var relayRegions = map[string]string{
	"KR": "asia",
	"JP": "asia",
	"SG": "asia",
	"IN": "asia",
	"DE": "europe",
	"FI": "europe",
	"SE": "europe",
}

func relayRegion(desc relay.Descriptor) string {
	return relayRegions[strings.ToUpper(desc.CountryCode)]
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

	represented := make(map[string]bool)
	for _, desc := range relays[:limit] {
		if region := relayRegion(desc); region != "" {
			represented[region] = true
		}
	}

	// Global order below the fold picks each missing region's best relay.
	var fills []int
	for i := limit; i < len(relays) && len(fills) < maxDiversitySlots; i++ {
		region := relayRegion(relays[i])
		if region == "" || represented[region] {
			continue
		}
		represented[region] = true
		fills = append(fills, i)
	}
	if len(fills) == 0 {
		return relays
	}

	out := append([]relay.Descriptor(nil), relays...)
	slot := limit - 1
	for _, fill := range fills {
		if slot < 1 {
			break
		}
		out[slot], out[fill] = out[fill], out[slot]
		slot--
	}
	return out
}
