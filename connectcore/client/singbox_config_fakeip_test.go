// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestFakeIPCacheAndBypass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		split    *SplitTunnelRules
		wantFake bool
	}{
		{"enabled", "/private/fakeip.db", nil, true},
		{"disabled", "", nil, false},
		{"lan", "/private/fakeip.db", &SplitTunnelRules{BypassLAN: true}, false},
		{"country", "/private/fakeip.db", &SplitTunnelRules{BypassCountries: []string{SplitTunnelCountryCN}, RuleSetDirectory: "/rules"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := buildDecoded(t, SingBoxConfigInput{Relay: validRelay(time.Now()), DNSShape: DNSShapeDoHFailover, ClashAPI: true, FakeIPCachePath: tc.path, SplitTunnel: tc.split})
			dns := cfg["dns"].(map[string]any)
			found := false
			for _, raw := range dns["servers"].([]any) {
				server := raw.(map[string]any)
				if server["type"] == "fakeip" {
					found = true
					if server["inet4_range"] != "198.18.0.0/19" || server["inet6_range"] != "fc00::/22" {
						t.Fatal(server)
					}
				}
			}
			if found != tc.wantFake {
				t.Fatalf("fake-IP enabled=%v", found)
			}
			experimental := cfg["experimental"].(map[string]any)
			if _, ok := experimental["clash_api"]; !ok {
				t.Fatal("lost traffic accounting")
			}
			cache, present := experimental["cache_file"]
			if present != tc.wantFake {
				t.Fatalf("cache enabled=%v", present)
			}
			if present && !reflect.DeepEqual(cache, map[string]any{"enabled": true, "path": tc.path, "store_fakeip": true}) {
				t.Fatal(cache)
			}
			// The full reserved ranges remain rejected before bypasses even
			// when this run uses real DNS and does not open the cache.
			rules := routeRulesOf(cfg)
			if !reflect.DeepEqual(rules[2], map[string]any{"ip_cidr": []any{"198.18.0.0/15", "fc00::/18"}, "action": "reject", "no_drop": true}) {
				t.Fatal(rules)
			}
			if tc.wantFake {
				rules := dns["rules"].([]any)
				// Four terminal probe rules precede the A/AAAA fake-IP rule.
				if !reflect.DeepEqual(rules[4], map[string]any{"query_type": []any{"A", "AAAA"}, "server": "fakeip", "rewrite_ttl": float64(1)}) {
					t.Fatal(rules)
				}
			}
		})
	}
}

func TestProxyModePassesHostnamesToTheRelay(t *testing.T) {
	cfg := buildDecoded(t, SingBoxConfigInput{Relay: validRelay(time.Now()), Mode: ModeProxy, ProxyListenPort: 7890, FakeIPCachePath: "/private/fakeip.db"})
	if cfg["experimental"] != nil {
		t.Fatal("proxy mode must not open the DNS cache")
	}
	for _, raw := range cfg["outbounds"].([]any) {
		if raw.(map[string]any)["domain_strategy"] != nil {
			t.Fatal("proxy outbound resolves hostnames locally")
		}
	}
	for _, rule := range routeRulesOf(cfg) {
		if rule["action"] == "resolve" || rule["override_address"] != nil {
			t.Fatal("proxy route rewrites destination")
		}
	}
}

func TestFakeIPBlocksAreDisjointAndInsideTheReservedRanges(t *testing.T) {
	var v4, v6 []netip.Prefix
	for n := range FakeIPBlockCount {
		inet4 := netip.MustParsePrefix(fakeIPBlockRange(fakeIPv4Range, n))
		inet6 := netip.MustParsePrefix(fakeIPBlockRange(fakeIPv6Range, n))
		if inet4.Bits() != 19 || !netip.MustParsePrefix(fakeIPv4Range).Contains(inet4.Addr()) || inet4 != inet4.Masked() {
			t.Fatalf("block %d: bad IPv4 range %s", n, inet4)
		}
		if inet6.Bits() != 22 || !netip.MustParsePrefix(fakeIPv6Range).Contains(inet6.Addr()) || inet6 != inet6.Masked() {
			t.Fatalf("block %d: bad IPv6 range %s", n, inet6)
		}
		for i := range v4 {
			if v4[i].Overlaps(inet4) || v6[i].Overlaps(inet6) {
				t.Fatalf("block %d overlaps block %d", n, i)
			}
		}
		v4, v6 = append(v4, inet4), append(v6, inet6)
	}
	if got := fakeIPBlockRange(fakeIPv4Range, FakeIPBlockCount-1); got != "198.19.224.0/19" {
		t.Fatal(got)
	}
	cfg := buildDecoded(t, SingBoxConfigInput{Relay: validRelay(time.Now()), FakeIPCachePath: "/private/fakeip.db", FakeIPBlock: 5})
	for _, raw := range cfg["dns"].(map[string]any)["servers"].([]any) {
		if server := raw.(map[string]any); server["type"] == "fakeip" && (server["inet4_range"] != "198.18.160.0/19" || server["inet6_range"] != "fc00:1400::/22") {
			t.Fatal(server)
		}
	}
	for _, block := range []int{-1, FakeIPBlockCount} {
		if _, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(time.Now()), FakeIPCachePath: "/private/fakeip.db", FakeIPBlock: block}); err == nil {
			t.Fatalf("accepted block %d", block)
		}
	}
}
