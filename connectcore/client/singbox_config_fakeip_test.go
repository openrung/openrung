// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"
)

// pinFakeIPBlock makes every config built during the test draw block n.
func pinFakeIPBlock(t *testing.T, n int) {
	t.Helper()
	previous := fakeIPBlock
	fakeIPBlock = func() int { return n }
	t.Cleanup(func() { fakeIPBlock = previous })
}

// buildRawDecoded is buildDecoded that also returns the emitted bytes.
func buildRawDecoded(t *testing.T, input SingBoxConfigInput) ([]byte, map[string]any) {
	t.Helper()
	raw, err := BuildSingBoxConfig(input)
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	return raw, decoded
}

func dnsServerByTag(t *testing.T, decoded map[string]any, tag string) map[string]any {
	t.Helper()
	for _, raw := range decoded["dns"].(map[string]any)["servers"].([]any) {
		server := raw.(map[string]any)
		if server["tag"] == tag {
			return server
		}
	}
	return nil
}

func fakeIPRuleIndex(rules []any) int {
	for i, raw := range rules {
		if raw.(map[string]any)["server"] == "fakeip" {
			return i
		}
	}
	return -1
}

func TestFakeIPBlocksAreDisjointAndInsideTheReservedRanges(t *testing.T) {
	var v4, v6 []netip.Prefix
	for n := range fakeIPBlockCount {
		inet4, inet6 := fakeIPRanges(n)
		if !inet4.Addr().Is4() || inet4.Bits() != 19 || !fakeIPv4Range.Contains(inet4.Addr()) || inet4 != inet4.Masked() {
			t.Fatalf("block %d: bad IPv4 range %s", n, inet4)
		}
		if !inet6.Addr().Is6() || inet6.Bits() != 22 || !fakeIPv6Range.Contains(inet6.Addr()) || inet6 != inet6.Masked() {
			t.Fatalf("block %d: bad IPv6 range %s", n, inet6)
		}
		for _, other := range v4 {
			if other.Overlaps(inet4) {
				t.Fatalf("block %d: IPv4 range %s overlaps %s", n, inet4, other)
			}
		}
		for _, other := range v6 {
			if other.Overlaps(inet6) {
				t.Fatalf("block %d: IPv6 range %s overlaps %s", n, inet6, other)
			}
		}
		v4, v6 = append(v4, inet4), append(v6, inet6)
	}
	if first, _ := fakeIPRanges(0); first.String() != "198.18.0.0/19" {
		t.Fatalf("block 0 = %s", first)
	}
	if last, lastV6 := fakeIPRanges(fakeIPBlockCount - 1); last.String() != "198.19.224.0/19" || lastV6.String() != "fc00:3c00::/22" {
		t.Fatalf("last block = %s, %s", last, lastV6)
	}
}

// The desktop/CLI TCP shape gains exactly a fakeip server and one A/AAAA
// rule; the real resolvers stay the final and the route's own resolver, so
// sing-box's internal lookups never see a fake address.
func TestFakeIPTUNTCPShape(t *testing.T) {
	pinFakeIPBlock(t, 0)
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	decoded := buildDecoded(t, SingBoxConfigInput{Relay: validRelay(now), FakeIP: true})

	fake := dnsServerByTag(t, decoded, "fakeip")
	if fake == nil || fake["type"] != "fakeip" || fake["inet4_range"] != "198.18.0.0/19" || fake["inet6_range"] != "fc00::/22" {
		t.Fatalf("fakeip server = %v", fake)
	}
	dns := decoded["dns"].(map[string]any)
	if dns["final"] != "dns-0" {
		t.Fatalf("dns final = %v", dns["final"])
	}
	want := []any{map[string]any{
		"query_type":  []any{"A", "AAAA"},
		"server":      "fakeip",
		"rewrite_ttl": float64(fakeIPTTL),
	}}
	if !reflect.DeepEqual(dns["rules"], want) {
		t.Fatalf("dns rules = %v", dns["rules"])
	}
	route := decoded["route"].(map[string]any)
	if route["default_domain_resolver"] != "dns-0" {
		t.Fatalf("default_domain_resolver = %v", route["default_domain_resolver"])
	}
	// Both hijack rules still lead the route, and the udp-443 reject still
	// closes it, so captured DNS reaches the fake-IP rule and QUIC to a fake
	// address is refused like any other.
	rules := route["rules"].([]any)
	if rules[0].(map[string]any)["action"] != "hijack-dns" || rules[1].(map[string]any)["port"] != float64(53) {
		t.Fatalf("hijack rules moved: %v", rules)
	}
	if last := rules[len(rules)-1].(map[string]any); last["action"] != "reject" || last["port"] != float64(443) {
		t.Fatalf("udp-443 reject no longer last: %v", last)
	}
}

// In the DoH failover shape the probe pins stay ahead of the fake-IP rule, so
// mobile's fresh-DNS probe still proves real resolution through the tunnel,
// and the failover chain follows it for every other query type.
func TestFakeIPDoHShapeKeepsProbePinsFirst(t *testing.T) {
	pinFakeIPBlock(t, 1)
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	decoded := buildDecoded(t, SingBoxConfigInput{
		Relay: validRelay(now), DNSShape: DNSShapeDoHFailover, FakeIP: true,
		SplitTunnel: &SplitTunnelRules{BypassLAN: true},
	})
	dns := decoded["dns"].(map[string]any)
	rules := dns["rules"].([]any)
	at := fakeIPRuleIndex(rules)
	if at < 0 {
		t.Fatalf("no fake-IP rule: %v", rules)
	}
	for i, raw := range rules {
		_, pinned := raw.(map[string]any)["domain_suffix"]
		if pinned != (i < at) {
			t.Fatalf("rule %d (fake-IP rule at %d): probe pins must all precede it: %v", i, at, rules)
		}
	}
	if at == len(rules)-1 {
		t.Fatal("failover chain no longer follows the fake-IP rule")
	}
	if dns["final"] != "dns-1" {
		t.Fatalf("dns final = %v", dns["final"])
	}
	if fake := dnsServerByTag(t, decoded, "fakeip"); fake == nil || fake["inet4_range"] != "198.18.32.0/19" {
		t.Fatalf("fakeip server = %v", fake)
	}
}

// Where fake IPs do not apply the request changes nothing: proxy mode already
// hands hostnames through, and country bypass needs real destination
// addresses for its geoip match.
func TestFakeIPIgnoredWhereItDoesNotApply(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := map[string]SingBoxConfigInput{
		"proxy": {Relay: validRelay(now), Mode: ModeProxy, ProxyListenPort: 7890},
		"country-bypass": {
			Relay: validRelay(now), DNSShape: DNSShapeDoHFailover,
			SplitTunnel: &SplitTunnelRules{BypassLAN: true, BypassCountries: []string{SplitTunnelCountryCN}, RuleSetDirectory: "/rules"},
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			without, _ := buildRawDecoded(t, input)
			input.FakeIP = true
			with, _ := buildRawDecoded(t, input)
			if !bytes.Equal(with, without) {
				t.Fatalf("FakeIP changed a config it does not apply to:\n%s", with)
			}
		})
	}
}

// Proxy mode resolves at the relay without fake IPs: the mixed inbound keeps
// the hostname a SOCKS5 or HTTP CONNECT client sends, and nothing on the way
// to the VLESS outbound may resolve it locally.
func TestProxyModePassesHostnamesToTheRelay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	decoded := buildDecoded(t, SingBoxConfigInput{Relay: validRelay(now), Mode: ModeProxy, ProxyListenPort: 7890})
	for _, raw := range decoded["outbounds"].([]any) {
		if strategy, ok := raw.(map[string]any)["domain_strategy"]; ok {
			t.Fatalf("outbound resolves locally (domain_strategy %v): %v", strategy, raw)
		}
	}
	for _, raw := range decoded["route"].(map[string]any)["rules"].([]any) {
		rule := raw.(map[string]any)
		if rule["action"] == "resolve" || rule["override_address"] != nil {
			t.Fatalf("route rule rewrites the destination: %v", rule)
		}
	}
	if inbound := decoded["inbounds"].([]any)[0].(map[string]any); inbound["domain_strategy"] != nil || inbound["sniff_override_destination"] != nil {
		t.Fatalf("inbound rewrites the destination: %v", inbound)
	}
}

// Each build draws its block afresh, so a reconnect's run does not reuse the
// previous run's addresses.
func TestFakeIPBlockDrawnPerBuild(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	input := SingBoxConfigInput{Relay: validRelay(now), FakeIP: true}
	draws := []int{5, 9}
	fakeIPBlockDraws := 0
	previous := fakeIPBlock
	fakeIPBlock = func() int { n := draws[fakeIPBlockDraws]; fakeIPBlockDraws++; return n }
	t.Cleanup(func() { fakeIPBlock = previous })

	var ranges []any
	for range draws {
		decoded := buildDecoded(t, input)
		ranges = append(ranges, dnsServerByTag(t, decoded, "fakeip")["inet4_range"])
	}
	if !slices.Equal(ranges, []any{"198.18.160.0/19", "198.19.32.0/19"}) {
		t.Fatalf("ranges = %v", ranges)
	}
	for range 64 {
		if n := previous(); n < 0 || n >= fakeIPBlockCount {
			t.Fatalf("draw %d out of range", n)
		}
	}
}
