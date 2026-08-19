package client

import (
	"errors"
	"fmt"
)

// SplitTunnelRules is the validated split-tunneling emission input (mobile's
// split-tunnel spec §2) — the Go twin of the Kotlin/Swift SplitTunnelRules.
// This is NOT the persisted mobile preference payload: the caller has already
// decided policy and verified that BOTH .srs files exist under
// RuleSetDirectory for every entry in BypassCountries. A nil
// SingBoxConfigInput.SplitTunnel means split tunneling is disabled and the
// emitted config is byte-identical to one built without the feature.
type SplitTunnelRules struct {
	// BypassLAN routes private-range destinations (ip_is_private) direct.
	BypassLAN bool
	// BypassCountries are entries of SplitTunnelSupportedCountries, already
	// normalized by the caller to that canonical order. Unknown or duplicate
	// codes are a build error: a duplicate rule-set tag fails sing-box's Start
	// stage anyway, and failing here names the actual mistake.
	BypassCountries []string
	// ExcludedPackages leave the VPN at the OS level via the TUN inbound's
	// exclude_package. Android-only — iOS has no OS-level per-app exclusion
	// and its callers leave this empty.
	ExcludedPackages []string
	// RuleSetDirectory is the absolute directory containing
	// geosite-<cc>.srs / geoip-<cc>.srs for every bypass country.
	RuleSetDirectory string
}

const (
	SplitTunnelCountryIR = "ir"
	SplitTunnelCountryCN = "cn"
)

// SplitTunnelSupportedCountries are the countries with bundled rule sets, in
// the canonical emission order.
var SplitTunnelSupportedCountries = []string{SplitTunnelCountryIR, SplitTunnelCountryCN}

// splitTunnelDirectResolver is an in-country public resolver reached over the
// DIRECT path, so bypassed domains resolve to in-country CDN nodes instead of
// the relay exit's view of them.
//
// DoH over 443, never plaintext UDP/53: these queries leave the device on the
// user's real IP while the tunnel is up, so the local network, the ISP and
// anything else on the path must not get a cleartext list of the domains being
// bypassed (nor the chance to forge the answers). server stays an IP literal
// so no bootstrap lookup is needed, and TLS authenticates tlsServerName — the
// same shape the proxied DoH resolvers use.
type splitTunnelDirectResolver struct {
	server        string
	tlsServerName string
}

// splitTunnelDirectResolvers maps every supported country to its direct-path
// resolver. A nil entry means the country has no encrypted public resolver we
// can currently stand behind: its bypass then keeps its ROUTE rules — the
// traffic still takes the direct path — and only its lookups fall to the
// proxied DoH chain, resolving through the relay exit's view. That costs
// in-country CDN affinity and nothing else.
//
// A resolver goes in here only while its certificate actually validates.
// Anything else is worse than omitting it: sing-box would spend the full
// evaluate timeout failing the TLS handshake on EVERY bypassed lookup before
// the fallback runs, so users would pay latency for an in-country answer they
// can never receive. Never restore one on the strength of its documentation —
// verify the live endpoint first.
var splitTunnelDirectResolvers = map[string]*splitTunnelDirectResolver{
	// Shecan is Iran's usual public resolver, but every endpoint it publishes
	// (178.22.122.100, 185.51.200.2, dns.shecan.ir) served an expired Let's
	// Encrypt certificate as of 2026-08-12 (notAfter Jul 10 2026), on both 443
	// and 853. Electro (78.157.42.100) refuses both ports and Begzar's DoH host
	// no longer resolves, so Iran has no verifiable encrypted resolver to point
	// at right now.
	SplitTunnelCountryIR: nil,
	// AliDNS (Chinese public resolver); https://223.5.5.5/dns-query is a
	// published endpoint and its certificate covers the IP literal.
	SplitTunnelCountryCN: {server: "223.5.5.5", tlsServerName: "dns.alidns.com"},
}

// validateSplitTunnel rejects split-tunnel inputs the emission below cannot
// represent safely. Bypass countries require the DoH failover DNS shape: the
// probe-priority pins that keep a country rule set (geosite-cn ships
// gstatic-class hosts) from capturing probe lookups only exist in that shape,
// and shipping country ROUTE rules without them recreates the confirmed
// CONNECTED-over-a-dead-tunnel regression mobile's tests guard against.
func validateSplitTunnel(input SingBoxConfigInput) error {
	rules := input.SplitTunnel
	if rules == nil || len(rules.BypassCountries) == 0 {
		return nil
	}
	if input.DNSShape != DNSShapeDoHFailover {
		return errors.New("split-tunnel bypass countries require DNSShapeDoHFailover")
	}
	if rules.RuleSetDirectory == "" {
		return errors.New("split-tunnel bypass countries require a rule set directory")
	}
	seen := make(map[string]bool, len(rules.BypassCountries))
	for _, country := range rules.BypassCountries {
		if _, supported := splitTunnelDirectResolvers[country]; !supported {
			return fmt.Errorf("unsupported split-tunnel country: %q", country)
		}
		if seen[country] {
			return fmt.Errorf("duplicate split-tunnel country: %q", country)
		}
		seen[country] = true
	}
	return nil
}
