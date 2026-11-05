// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

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
	for n := range FakeIPBlockCount {
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
	if last, lastV6 := fakeIPRanges(FakeIPBlockCount - 1); last.String() != "198.19.224.0/19" || lastV6.String() != "fc00:3c00::/22" {
		t.Fatalf("last block = %s, %s", last, lastV6)
	}
}

// The desktop/CLI TCP shape gains a fakeip server answering from the chosen
// block and one A/AAAA rule; the real resolvers stay the final and the
// route's own resolver, so sing-box's internal lookups never see a fake
// address.
func TestFakeIPTUNTCPShape(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	_, decoded := buildRawDecoded(t, SingBoxConfigInput{Relay: validRelay(now), FakeIP: true, FakeIPBlock: 5})

	fake := dnsServerByTag(t, decoded, "fakeip")
	if fake == nil || fake["type"] != "fakeip" || fake["inet4_range"] != "198.18.160.0/19" || fake["inet6_range"] != "fc00:1400::/22" {
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
	// Both hijack rules still lead the route, the stale-address reject
	// follows them, and the udp-443 reject still closes the list.
	rules := route["rules"].([]any)
	if rules[0].(map[string]any)["action"] != "hijack-dns" || rules[1].(map[string]any)["port"] != float64(53) {
		t.Fatalf("hijack rules moved: %v", rules)
	}
	wantStale := map[string]any{
		"ip_cidr": []any{"198.18.0.0/15", "fc00::/18"},
		"action":  "reject",
		"no_drop": true,
	}
	if !reflect.DeepEqual(rules[2], wantStale) {
		t.Fatalf("stale fake-address reject = %v", rules[2])
	}
	if last := rules[len(rules)-1].(map[string]any); last["action"] != "reject" || last["port"] != float64(443) {
		t.Fatalf("udp-443 reject no longer last: %v", last)
	}
}

// In the DoH failover shape the probe pins stay ahead of the fake-IP rule, so
// mobile's fresh-DNS probe still proves real resolution through the tunnel,
// and the failover chain follows it for every other query type.
func TestFakeIPDoHShapeKeepsProbePinsFirst(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	_, decoded := buildRawDecoded(t, SingBoxConfigInput{
		Relay: validRelay(now), DNSShape: DNSShapeDoHFailover, FakeIP: true, FakeIPBlock: 1,
		SplitTunnel: &SplitTunnelRules{ExcludedPackages: []string{"com.example.bypass"}},
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

// Proxy mode already hands hostnames through, so FakeIP changes nothing
// there; RejectFakeIPRanges is TUN-only too.
func TestFakeIPIgnoredInProxyMode(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	input := SingBoxConfigInput{Relay: validRelay(now), Mode: ModeProxy, ProxyListenPort: 7890}
	without, _ := buildRawDecoded(t, input)
	input.FakeIP, input.FakeIPBlock, input.RejectFakeIPRanges = true, 7, true
	with, _ := buildRawDecoded(t, input)
	if !bytes.Equal(with, without) {
		t.Fatalf("fake-IP inputs changed a proxy-mode config:\n%s", with)
	}
}

// The LAN and country bypasses need real destination addresses, so they hand
// out no fake addresses — but a TUN run asked for fake IPs, or told some may
// still be cached, keeps refusing the fake ranges ahead of every bypass rule,
// and that refusal is the only difference.
func TestFakeIPSuppressedByBypassKeepsStaleRefusal(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := map[string]SingBoxConfigInput{
		"lan-bypass": {
			Relay: validRelay(now), DNSShape: DNSShapeDoHFailover,
			SplitTunnel: &SplitTunnelRules{BypassLAN: true},
		},
		"country-bypass": {
			Relay: validRelay(now), DNSShape: DNSShapeDoHFailover,
			SplitTunnel: &SplitTunnelRules{BypassLAN: true, BypassCountries: []string{SplitTunnelCountryCN}, RuleSetDirectory: "/rules"},
		},
		"fake-ip-off": {Relay: validRelay(now)},
	}
	for name, base := range cases {
		requests := map[string]func(*SingBoxConfigInput){
			"fakeip":      func(in *SingBoxConfigInput) { in.FakeIP, in.FakeIPBlock = true, 7 },
			"stale-guard": func(in *SingBoxConfigInput) { in.RejectFakeIPRanges = true },
		}
		for request, apply := range requests {
			if name == "fake-ip-off" && request == "fakeip" {
				continue
			}
			t.Run(name+"/"+request, func(t *testing.T) {
				_, without := buildRawDecoded(t, base)
				input := base
				apply(&input)
				raw, with := buildRawDecoded(t, input)
				if bytes.Contains(raw, []byte(`"fakeip"`)) {
					t.Fatalf("fake-IP DNS emitted where it does not apply:\n%s", raw)
				}
				rules := with["route"].(map[string]any)["rules"].([]any)
				want := map[string]any{
					"ip_cidr": []any{"198.18.0.0/15", "fc00::/18"},
					"action":  "reject",
					"no_drop": true,
				}
				if !reflect.DeepEqual(rules[2], want) {
					t.Fatalf("rule 2 = %v, want the stale fake-range refusal", rules[2])
				}
				with["route"].(map[string]any)["rules"] = append(append([]any{}, rules[:2]...), rules[3:]...)
				if !reflect.DeepEqual(with, without) {
					t.Fatal("more than the stale refusal changed")
				}
			})
		}
	}
}

func TestFakeIPBlockOutOfRangeRejected(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	for _, block := range []int{-1, FakeIPBlockCount} {
		_, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(now), FakeIP: true, FakeIPBlock: block})
		if err == nil || !strings.Contains(err.Error(), "fake-IP block") {
			t.Fatalf("block %d: err = %v", block, err)
		}
	}
	// Without FakeIP the block is ignored, like every other fake-IP input.
	if _, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(now), FakeIPBlock: -1}); err != nil {
		t.Fatalf("FakeIPBlock validated with FakeIP off: %v", err)
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
