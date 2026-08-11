package client

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBuildSingBoxConfig(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(now)})
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbounds := decoded["inbounds"].([]any)
	tun := inbounds[0].(map[string]any)
	if tun["type"] != "tun" || tun["auto_route"] != true || tun["strict_route"] != true {
		t.Fatalf("unexpected tun inbound: %+v", tun)
	}
	if tun["dns_mode"] != "hijack" {
		t.Fatalf("expected TUN DNS hijack mode, got %+v", tun["dns_mode"])
	}

	outbounds := decoded["outbounds"].([]any)
	proxy := outbounds[0].(map[string]any)
	if proxy["type"] != "vless" || proxy["server"] != "relay.example.com" {
		t.Fatalf("unexpected proxy outbound: %+v", proxy)
	}
	if proxy["server_port"].(float64) != 443 {
		t.Fatalf("unexpected server port: %+v", proxy["server_port"])
	}

	tls := proxy["tls"].(map[string]any)
	reality := tls["reality"].(map[string]any)
	if tls["server_name"] != "www.cloudflare.com" ||
		reality["public_key"] != "public-key" ||
		reality["short_id"] != "5f7a8d9c01ab23cd" {
		t.Fatalf("unexpected reality TLS config: %+v", tls)
	}

	route := decoded["route"].(map[string]any)
	rules := route["rules"].([]any)
	dnsRule := rules[0].(map[string]any)
	if dnsRule["protocol"] != "dns" || dnsRule["action"] != "hijack-dns" {
		t.Fatalf("expected DNS hijack rule, got %+v", dnsRule)
	}
	if route["final"] != "proxy" {
		t.Fatalf("expected proxy final route, got %+v", route["final"])
	}
	if route["default_domain_resolver"] != "dns-0" {
		t.Fatalf("expected dns-0 default domain resolver, got %+v", route["default_domain_resolver"])
	}

	dns := decoded["dns"].(map[string]any)
	servers := dns["servers"].([]any)
	dns0 := servers[0].(map[string]any)
	if dns0["type"] != "tcp" || dns0["detour"] != "proxy" {
		t.Fatalf("expected TCP DNS through proxy, got %+v", dns0)
	}
}

func TestBuildSingBoxConfigExcludesIPv6RelayFromTUNRoute(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	relay := validRelay(now)
	relay.PublicHost = "2001:db8::443"

	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: relay})
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbounds := decoded["inbounds"].([]any)
	tun := inbounds[0].(map[string]any)
	excluded := tun["route_exclude_address"].([]any)
	if len(excluded) != 1 || excluded[0] != "2001:db8::443/128" {
		t.Fatalf("expected IPv6 relay route exclusion, got %+v", excluded)
	}
}

func TestBuildSingBoxConfigExcludesIPv4RelayFromTUNRoute(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	relay := validRelay(now)
	relay.PublicHost = "203.0.113.10"

	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: relay})
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbounds := decoded["inbounds"].([]any)
	tun := inbounds[0].(map[string]any)
	excluded := tun["route_exclude_address"].([]any)
	if len(excluded) != 1 || excluded[0] != "203.0.113.10/32" {
		t.Fatalf("expected IPv4 relay route exclusion, got %+v", excluded)
	}
}

func TestBuildSingBoxConfigPunchBridgeRedirectsAndExcludesPeer(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	rly := validRelay(now)
	rly.PublicHost = "203.0.113.10" // the hub for a tunnel relay

	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{
		Relay:                   rly,
		BridgeHost:              "127.0.0.1",
		BridgePort:              54321,
		PunchPeerExcludeAddress: "198.51.100.7", // the relay's reflexive IP
	})
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	// The outbound must dial the loopback bridge, not the relay endpoint...
	proxy := decoded["outbounds"].([]any)[0].(map[string]any)
	if proxy["server"] != "127.0.0.1" || proxy["server_port"].(float64) != 54321 {
		t.Fatalf("outbound not pointed at the bridge: %+v", proxy)
	}
	// ...but the Reality identity must be unchanged (still targets the relay).
	reality := proxy["tls"].(map[string]any)["reality"].(map[string]any)
	if reality["public_key"] != "public-key" {
		t.Fatalf("reality identity changed on punch path: %+v", reality)
	}

	// Correction #1: the relay's reflexive peer IP MUST be excluded from the
	// TUN route so the QUIC datagrams are not captured and looped.
	tun := decoded["inbounds"].([]any)[0].(map[string]any)
	excluded := tun["route_exclude_address"].([]any)
	found := false
	for _, e := range excluded {
		if e == "198.51.100.7/32" {
			found = true
		}
	}
	if !found {
		t.Fatalf("punch peer IP not excluded from TUN route: %+v", excluded)
	}
}

func TestBuildSingBoxProxyModeUsesMixedInbound(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{
		Relay:           validRelay(now),
		Mode:            ModeProxy,
		ProxyListenPort: 7890,
	})
	if err != nil {
		t.Fatalf("build proxy config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbound := decoded["inbounds"].([]any)[0].(map[string]any)
	if inbound["type"] != "mixed" || inbound["tag"] != "mixed-in" {
		t.Fatalf("expected mixed inbound, got %+v", inbound)
	}
	if inbound["listen"] != "127.0.0.1" || inbound["listen_port"].(float64) != 7890 {
		t.Fatalf("expected loopback 7890, got listen=%v port=%v", inbound["listen"], inbound["listen_port"])
	}
	// A mixed inbound must never carry TUN-only keys.
	for _, key := range []string{"auto_route", "strict_route", "route_exclude_address", "dns_mode"} {
		if _, ok := inbound[key]; ok {
			t.Fatalf("proxy inbound leaked TUN-only key %q: %+v", key, inbound)
		}
	}
	// The outbound (VLESS+Reality) and final route are shared with TUN mode.
	proxy := decoded["outbounds"].([]any)[0].(map[string]any)
	if proxy["type"] != "vless" || proxy["server"] != "relay.example.com" {
		t.Fatalf("proxy mode should keep the VLESS outbound: %+v", proxy)
	}
	if decoded["route"].(map[string]any)["final"] != "proxy" {
		t.Fatalf("expected final route proxy")
	}
}

func TestBuildSingBoxProxyModeRequiresPort(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	_, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(now), Mode: ModeProxy})
	if err == nil {
		t.Fatal("expected an error when proxy mode has no listen port")
	}
}

func TestBuildSingBoxProxyModeHonorsPunchBridge(t *testing.T) {
	// Proxy mode still redirects the outbound to the loopback punch bridge when
	// set — punch is orthogonal to the inbound type.
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{
		Relay:           validRelay(now),
		Mode:            ModeProxy,
		ProxyListenPort: 7890,
		BridgeHost:      "127.0.0.1",
		BridgePort:      54321,
	})
	if err != nil {
		t.Fatalf("build proxy config: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	proxy := decoded["outbounds"].([]any)[0].(map[string]any)
	if proxy["server"] != "127.0.0.1" || proxy["server_port"].(float64) != 54321 {
		t.Fatalf("proxy mode did not honor the punch bridge: %+v", proxy)
	}
}

func TestBuildSingBoxConfigDoesNotExcludeHostnameRelay(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{Relay: validRelay(now)})
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbounds := decoded["inbounds"].([]any)
	tun := inbounds[0].(map[string]any)
	if _, ok := tun["route_exclude_address"]; ok {
		t.Fatalf("hostname relay should not get a literal route exclusion: %+v", tun)
	}
}

func TestBuildSingBoxConfigSplitTunnelNilAndInertPreserveBaseline(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	input := SingBoxConfigInput{Relay: validRelay(now), Mode: ModeProxy, ProxyListenPort: 7890}
	baseline, err := BuildSingBoxConfig(input)
	if err != nil {
		t.Fatalf("build baseline: %v", err)
	}

	for name, rules := range map[string]*SplitTunnelRules{
		"explicit nil": nil,
		"empty":        {},
		"unknown country": {
			BypassCountries:  []string{"xx"},
			RuleSetDirectory: "/var/rulesets",
		},
		"country without staged directory": {
			BypassCountries: []string{"ir"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			input.SplitTunnel = rules
			got, err := BuildSingBoxConfig(input)
			if err != nil {
				t.Fatalf("build split config: %v", err)
			}
			if !bytes.Equal(got, baseline) {
				t.Fatalf("inert split config changed baseline\nwant:\n%s\ngot:\n%s", baseline, got)
			}
		})
	}
}

func TestBuildSingBoxConfigSplitTunnelLANOnly(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{
		Relay:           validRelay(now),
		Mode:            ModeProxy,
		ProxyListenPort: 7890,
		SplitTunnel:     &SplitTunnelRules{BypassLAN: true},
	})
	if err != nil {
		t.Fatalf("build split config: %v", err)
	}
	decoded := decodeSingBoxConfig(t, cfg)
	route := decoded["route"].(map[string]any)
	rules := route["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("route rules = %+v, want baseline + resolve + LAN rule", rules)
	}
	if got := rules[1].(map[string]any); got["action"] != "resolve" {
		t.Fatalf("hostname destinations must resolve before LAN matching: %+v", got)
	}
	if got := rules[2].(map[string]any); got["ip_is_private"] != true || got["outbound"] != "direct" {
		t.Fatalf("unexpected LAN rule: %+v", got)
	}
	if _, ok := route["rule_set"]; ok {
		t.Fatalf("LAN-only split should not emit rule sets: %+v", route)
	}
	if _, ok := decoded["dns"].(map[string]any)["rules"]; ok {
		t.Fatalf("LAN-only split should leave DNS unchanged: %+v", decoded["dns"])
	}
}

func TestBuildSingBoxConfigSplitTunnelCountriesCanonicalAndPinned(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ruleDirectory := filepath.Join(string(filepath.Separator), "var", "rulesets")
	cfg, err := BuildSingBoxConfig(SingBoxConfigInput{
		Relay:           validRelay(now),
		Mode:            ModeProxy,
		ProxyListenPort: 7890,
		SplitTunnel: &SplitTunnelRules{
			BypassLAN:           true,
			BypassCountries:     []string{"cn", "IR", "cn", "unknown"},
			RuleSetDirectory:    ruleDirectory,
			ProxyDomainSuffixes: []string{"WWW.GSTATIC.COM", ".cp.cloudflare.com", "www.gstatic.com"},
		},
	})
	if err != nil {
		t.Fatalf("build split config: %v", err)
	}
	decoded := decodeSingBoxConfig(t, cfg)

	route := decoded["route"].(map[string]any)
	routeRules := route["rules"].([]any)
	if len(routeRules) != 7 {
		t.Fatalf("route rule count = %d, want 7: %+v", len(routeRules), routeRules)
	}
	if got := routeRules[1].(map[string]any)["action"]; got != "sniff" {
		t.Fatalf("country rules must start with sniff, got %v", got)
	}
	pin := routeRules[2].(map[string]any)
	if pin["outbound"] != "proxy" || !reflect.DeepEqual(pin["domain_suffix"], []any{"www.gstatic.com", "cp.cloudflare.com"}) {
		t.Fatalf("unexpected probe pin: %+v", pin)
	}
	if got := routeRules[3].(map[string]any); got["action"] != "resolve" {
		t.Fatalf("hostname destinations must resolve before IP rule sets: %+v", got)
	}
	if got := routeRules[4].(map[string]any); got["ip_is_private"] != true || got["outbound"] != "direct" {
		t.Fatalf("LAN rule should precede countries: %+v", got)
	}
	for index, want := range [][]any{{"geosite-ir", "geoip-ir"}, {"geosite-cn", "geoip-cn"}} {
		got := routeRules[index+5].(map[string]any)
		if got["outbound"] != "direct" || !reflect.DeepEqual(got["rule_set"], want) {
			t.Fatalf("country rule %d = %+v, want %v direct", index, got, want)
		}
	}
	ruleSets := route["rule_set"].([]any)
	wantTags := []string{"geosite-ir", "geoip-ir", "geosite-cn", "geoip-cn"}
	for index, wantTag := range wantTags {
		got := ruleSets[index].(map[string]any)
		if got["type"] != "local" || got["format"] != "binary" || got["tag"] != wantTag || got["path"] != filepath.Join(ruleDirectory, wantTag+".srs") {
			t.Fatalf("rule set %d = %+v", index, got)
		}
	}

	dns := decoded["dns"].(map[string]any)
	dnsServers := dns["servers"].([]any)
	if len(dnsServers) != 4 {
		t.Fatalf("DNS servers = %d, want 4: %+v", len(dnsServers), dnsServers)
	}
	for index, want := range []struct{ tag, server string }{{"dns-direct-ir", "178.22.122.100"}, {"dns-direct-cn", "223.5.5.5"}} {
		got := dnsServers[index+2].(map[string]any)
		if got["tag"] != want.tag || got["type"] != "udp" || got["server"] != want.server {
			t.Fatalf("direct DNS server %d = %+v", index, got)
		}
		if _, ok := got["detour"]; ok {
			t.Fatalf("direct DNS server must not have detour: %+v", got)
		}
	}
	dnsRules := dns["rules"].([]any)
	if len(dnsRules) != 3 {
		t.Fatalf("DNS rules = %+v, want probe pin + IR + CN", dnsRules)
	}
	if got := dnsRules[0].(map[string]any); got["server"] != "dns-0" || !reflect.DeepEqual(got["domain_suffix"], []any{"www.gstatic.com", "cp.cloudflare.com"}) {
		t.Fatalf("probe DNS pin must be first: %+v", got)
	}
	for index, want := range []string{"geosite-ir", "geosite-cn"} {
		got := dnsRules[index+1].(map[string]any)
		if !reflect.DeepEqual(got["rule_set"], []any{want}) {
			t.Fatalf("DNS country order mismatch: %+v", dnsRules)
		}
	}

	inbound := decoded["inbounds"].([]any)[0].(map[string]any)
	for _, key := range []string{"auto_route", "strict_route", "route_exclude_address", "dns_mode", "exclude_package", "include_package"} {
		if _, ok := inbound[key]; ok {
			t.Fatalf("proxy split inbound leaked %q: %+v", key, inbound)
		}
	}
}

func decodeSingBoxConfig(t *testing.T, config []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	return decoded
}
