// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
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
					if server["inet4_range"] != "198.18.0.0/15" || server["inet6_range"] != "fc00::/18" {
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
