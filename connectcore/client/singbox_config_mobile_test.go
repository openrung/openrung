package client

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// The tests below mirror the assertions in mobile's own generator tests
// (SingBoxConfigurationDnsTest, SingBoxConfigurationSplitTunnelTest,
// SingBoxConfigurationPunchTest), so the Go builder provably reproduces the
// emissions mobile expects before it is bound in.

func mobileRelay(t *testing.T) brokerapi.RelayDescriptor {
	t.Helper()
	r := validRelay(time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC))
	r.PublicHost = "203.0.113.10"
	return r
}

func mobileInput(r brokerapi.RelayDescriptor) SingBoxConfigInput {
	return SingBoxConfigInput{
		Relay:    r,
		MTU:      1400,
		LogLevel: "warn",
		DNSShape: DNSShapeDoHFailover,
		ClashAPI: true,
	}
}

func buildDecoded(t *testing.T, input SingBoxConfigInput) map[string]any {
	t.Helper()
	cfg, err := BuildSingBoxConfig(input)
	if err != nil {
		t.Fatalf("build sing-box config: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	return decoded
}

func dnsRulesOf(decoded map[string]any) []map[string]any {
	raw := decoded["dns"].(map[string]any)["rules"].([]any)
	rules := make([]map[string]any, len(raw))
	for i, r := range raw {
		rules[i] = r.(map[string]any)
	}
	return rules
}

func routeRulesOf(decoded map[string]any) []map[string]any {
	raw := decoded["route"].(map[string]any)["rules"].([]any)
	rules := make([]map[string]any, len(raw))
	for i, r := range raw {
		rules[i] = r.(map[string]any)
	}
	return rules
}

func TestMobileNilAndEmptySplitTunnelEmitIdenticalBytes(t *testing.T) {
	base := mobileInput(mobileRelay(t))
	baseline, err := BuildSingBoxConfig(base)
	if err != nil {
		t.Fatalf("build baseline: %v", err)
	}
	withEmpty := base
	withEmpty.SplitTunnel = &SplitTunnelRules{}
	emptied, err := BuildSingBoxConfig(withEmpty)
	if err != nil {
		t.Fatalf("build with empty split rules: %v", err)
	}
	if !bytes.Equal(baseline, emptied) {
		t.Fatal("all-empty split rules must emit the exact no-split configuration")
	}
}

func TestMobileDoHServersAreEncryptedAndProxied(t *testing.T) {
	input := mobileInput(mobileRelay(t))
	input.SplitTunnel = &SplitTunnelRules{
		BypassCountries:  []string{SplitTunnelCountryIR, SplitTunnelCountryCN},
		RuleSetDirectory: "/data/user/0/rulesets",
	}
	decoded := buildDecoded(t, input)

	servers := decoded["dns"].(map[string]any)["servers"].([]any)
	tags := make([]string, 0, len(servers))
	for _, s := range servers {
		server := s.(map[string]any)
		tag := server["tag"].(string)
		tags = append(tags, tag)
		// Proxied resolvers need DoH because TCP/53 gets no replies under WSS;
		// the direct bypass resolvers need it so a cleartext, forgeable domain
		// list never leaves the device on the user's real IP. Neither may ever
		// regress to udp/tcp.
		if server["type"] != "https" {
			t.Fatalf("dns server %s is not DoH: %+v", tag, server)
		}
		if server["tls"].(map[string]any)["server_name"] == "" {
			t.Fatalf("dns server %s does not authenticate a hostname", tag)
		}
		if strings.HasPrefix(tag, "dns-direct-") {
			// A direct resolver with a detour to the empty tagged direct
			// outbound is rejected during sing-box's Start stage.
			if _, ok := server["detour"]; ok {
				t.Fatalf("direct resolver %s must build its own dialer: %+v", tag, server)
			}
		} else if server["detour"] != "proxy" {
			t.Fatalf("proxied resolver %s must detour through the proxy: %+v", tag, server)
		}
		for _, key := range []string{"server_port", "path", "domain_resolver"} {
			if _, ok := server[key]; ok {
				t.Fatalf("dns server %s must keep the %s default: %+v", tag, key, server)
			}
		}
	}
	// Iran contributes no resolver (no verifiable encrypted endpoint today);
	// only China does.
	if !reflect.DeepEqual(tags, []string{"dns-0", "dns-1", "dns-direct-cn"}) {
		t.Fatalf("unexpected dns server tags: %v", tags)
	}

	dns := decoded["dns"].(map[string]any)
	if dns["final"] != "dns-1" || dns["timeout"] != "3s" {
		t.Fatalf("expected terminal fallback final and 3s timeout, got final=%v timeout=%v", dns["final"], dns["timeout"])
	}
	if decoded["route"].(map[string]any)["default_domain_resolver"] != "dns-0" {
		t.Fatal("default domain resolver must stay on the primary")
	}
}

func TestMobileProbeDNSChainIsFirstUncachedAndTerminal(t *testing.T) {
	baseline := mobileInput(mobileRelay(t))
	china := mobileInput(mobileRelay(t))
	china.SplitTunnel = &SplitTunnelRules{
		BypassCountries:  []string{SplitTunnelCountryCN},
		RuleSetDirectory: "/data/user/0/rulesets",
	}

	for _, input := range []SingBoxConfigInput{baseline, china} {
		rules := dnsRulesOf(buildDecoded(t, input))
		probeChain := rules[:4]
		for _, rule := range probeChain {
			suffixes := rule["domain_suffix"].([]any)
			if len(suffixes) != 2 || suffixes[0] != "probe.openrung.org" || suffixes[1] != "cp.cloudflare.com" {
				t.Fatalf("probe rule not scoped to the probe domains: %+v", rule)
			}
			if rule["match_response"] != true {
				// A cached answer proves nothing about the tunnel right now.
				if rule["disable_cache"] != true || rule["disable_optimistic_cache"] != true {
					t.Fatalf("probe rule must disable caching: %+v", rule)
				}
			}
		}
		if probeChain[0]["action"] != "evaluate" || probeChain[0]["server"] != "dns-0" {
			t.Fatalf("probe chain must start by evaluating the primary: %+v", probeChain[0])
		}
		// Nonce probes legitimately draw NXDOMAIN; the primary answering one
		// must respond.
		if probeChain[2]["response_rcode"] != "NXDOMAIN" {
			t.Fatalf("probe chain must respond to NXDOMAIN: %+v", probeChain[2])
		}
		// The trailing route rule is terminal (no action key), so a probe
		// lookup can never leak past it into a country rule.
		if _, ok := probeChain[3]["action"]; ok {
			t.Fatalf("probe chain terminus must be a route rule: %+v", probeChain[3])
		}
		if probeChain[3]["server"] != "dns-1" {
			t.Fatalf("probe chain terminus must name the fallback: %+v", probeChain[3])
		}
	}
}

func TestMobileChinaBypassNeverOutranksProbePins(t *testing.T) {
	input := mobileInput(mobileRelay(t))
	input.SplitTunnel = &SplitTunnelRules{
		BypassCountries:  []string{SplitTunnelCountryCN},
		RuleSetDirectory: "/data/user/0/rulesets",
	}
	decoded := buildDecoded(t, input)

	// The confirmed regression this guards: geosite-cn contains
	// www.gstatic.com, so without the pins a dead proxy still produced a
	// passing probe over the direct path and the app published CONNECTED.
	dnsRules := dnsRulesOf(decoded)
	countryIndex := -1
	for i, rule := range dnsRules {
		if _, ok := rule["rule_set"]; ok {
			countryIndex = i
			break
		}
	}
	if countryIndex != 4 {
		t.Fatalf("country dns rule must follow the 4-rule probe chain, got index %d", countryIndex)
	}

	routeRules := routeRulesOf(decoded)
	probeIndex, bypassIndex := -1, -1
	for i, rule := range routeRules {
		if _, ok := rule["domain_suffix"]; ok && rule["outbound"] == "proxy" && probeIndex < 0 {
			probeIndex = i
		}
		if rule["outbound"] == "direct" && bypassIndex < 0 {
			bypassIndex = i
		}
	}
	if probeIndex < 0 || bypassIndex < probeIndex {
		t.Fatalf("probe route pin must exist and precede every direct-bypass rule: probe=%d bypass=%d", probeIndex, bypassIndex)
	}
	// Route rules need a sniffed domain before geosite matching can work.
	if routeRules[1]["action"] != "sniff" {
		t.Fatalf("sniff must precede the probe pin: %+v", routeRules[1])
	}
}

func TestMobileSplitRuleOrderWithBothCountriesAndLAN(t *testing.T) {
	input := mobileInput(mobileRelay(t))
	input.SplitTunnel = &SplitTunnelRules{
		BypassLAN:        true,
		BypassCountries:  []string{SplitTunnelCountryIR, SplitTunnelCountryCN},
		RuleSetDirectory: "/data/user/0/rulesets",
	}
	decoded := buildDecoded(t, input)

	rules := routeRulesOf(decoded)
	if len(rules) != 6 {
		t.Fatalf("expected the 6-rule canonical order, got %d: %+v", len(rules), rules)
	}
	if rules[0]["action"] != "hijack-dns" || rules[1]["action"] != "sniff" ||
		rules[2]["outbound"] != "proxy" || rules[3]["ip_is_private"] != true {
		t.Fatalf("canonical rule order broken: %+v", rules)
	}
	for i, want := range map[int]string{4: "geosite-ir", 5: "geosite-cn"} {
		ruleSet := rules[i]["rule_set"].([]any)
		if ruleSet[0] != want || rules[i]["outbound"] != "direct" {
			t.Fatalf("country rule %d unexpected: %+v", i, rules[i])
		}
	}

	ruleSets := decoded["route"].(map[string]any)["rule_set"].([]any)
	var tags []string
	for _, rs := range ruleSets {
		object := rs.(map[string]any)
		tags = append(tags, object["tag"].(string))
		if object["type"] != "local" || object["format"] != "binary" {
			t.Fatalf("rule set must be a local binary .srs: %+v", object)
		}
		if object["path"] != "/data/user/0/rulesets/"+object["tag"].(string)+".srs" {
			t.Fatalf("rule set path unexpected: %+v", object)
		}
	}
	if !reflect.DeepEqual(tags, []string{"geosite-ir", "geoip-ir", "geosite-cn", "geoip-cn"}) {
		t.Fatalf("unexpected rule set tags: %v", tags)
	}
}

func TestMobileExcludedPackagesLandOnTUNOnly(t *testing.T) {
	input := mobileInput(mobileRelay(t))
	input.SplitTunnel = &SplitTunnelRules{
		ExcludedPackages: []string{"com.tencent.mm", "org.telegram.messenger"},
	}
	decoded := buildDecoded(t, input)

	tun := decoded["inbounds"].([]any)[0].(map[string]any)
	excluded := tun["exclude_package"].([]any)
	if len(excluded) != 2 || excluded[0] != "com.tencent.mm" {
		t.Fatalf("unexpected exclude_package: %+v", excluded)
	}
	// Android forbids mixing the two, and we only ever exclude.
	if _, ok := tun["include_package"]; ok {
		t.Fatalf("include_package must never be emitted: %+v", tun)
	}
}

func TestMobileBridgeOmitsRouteExclusionsButKeepsSplitRules(t *testing.T) {
	splitTunnel := &SplitTunnelRules{
		BypassLAN:        true,
		BypassCountries:  []string{SplitTunnelCountryIR, SplitTunnelCountryCN},
		RuleSetDirectory: "/data/user/0/rulesets",
	}
	direct := mobileInput(mobileRelay(t))
	direct.SplitTunnel = splitTunnel
	bridged := direct
	bridged.BridgeHost = "127.0.0.1"
	bridged.BridgePort = 54321
	bridged.BridgeOwnsOuterSocket = true

	directDecoded := buildDecoded(t, direct)
	bridgedDecoded := buildDecoded(t, bridged)

	// DNS and route blocks are identical across the direct and bridged shapes.
	if !reflect.DeepEqual(directDecoded["dns"], bridgedDecoded["dns"]) {
		t.Fatal("dns block must be identical across direct and bridged shapes")
	}
	if !reflect.DeepEqual(directDecoded["route"], bridgedDecoded["route"]) {
		t.Fatal("route block must be identical across direct and bridged shapes")
	}

	// Leak-precedent regression guard: the punch/WSS loopback adapter must
	// never regain a peer /32 exclusion because split tunneling is on —
	// VpnService.protect(fd) exempts only the transport's own socket, so an
	// exclusion would leak unrelated apps' traffic to that IP.
	bridgedTUN := bridgedDecoded["inbounds"].([]any)[0].(map[string]any)
	if _, ok := bridgedTUN["route_exclude_address"]; ok {
		t.Fatalf("protected bridge must not emit route exclusions: %+v", bridgedTUN)
	}
	directTUN := directDecoded["inbounds"].([]any)[0].(map[string]any)
	if _, ok := directTUN["route_exclude_address"]; !ok {
		t.Fatalf("ordinary relay path must keep its endpoint route exclusion: %+v", directTUN)
	}

	// The bridge changes only the transport endpoint; the Reality identity
	// still targets the real relay.
	outbound := bridgedDecoded["outbounds"].([]any)[0].(map[string]any)
	if outbound["server"] != "127.0.0.1" || outbound["server_port"].(float64) != 54321 {
		t.Fatalf("outbound not pointed at the bridge: %+v", outbound)
	}
	if outbound["tls"].(map[string]any)["server_name"] != "www.cloudflare.com" {
		t.Fatalf("reality identity changed on the bridge path: %+v", outbound)
	}
}

func TestMobileIranContributesNoDNSServerOrRules(t *testing.T) {
	baseline := mobileInput(mobileRelay(t))
	iran := mobileInput(mobileRelay(t))
	iran.SplitTunnel = &SplitTunnelRules{
		BypassCountries:  []string{SplitTunnelCountryIR},
		RuleSetDirectory: "/data/user/0/rulesets",
	}
	baselineDecoded := buildDecoded(t, baseline)
	iranDecoded := buildDecoded(t, iran)

	// Iran's ROUTE bypass is untouched, but with no verifiable encrypted
	// in-country resolver it contributes no dns server and no dns rules — a
	// dead primary would burn the full evaluate timeout on every bypassed
	// lookup for an answer it can never receive.
	if !reflect.DeepEqual(baselineDecoded["dns"], iranDecoded["dns"]) {
		t.Fatal("an IR bypass must leave the dns block untouched")
	}
	rules := routeRulesOf(iranDecoded)
	last := rules[len(rules)-1]
	ruleSet := last["rule_set"].([]any)
	if ruleSet[0] != "geosite-ir" || last["outbound"] != "direct" {
		t.Fatalf("IR route bypass missing: %+v", last)
	}
}

func TestSplitTunnelValidation(t *testing.T) {
	base := mobileInput(mobileRelay(t))
	for name, mutate := range map[string]func(*SingBoxConfigInput){
		"unknown country": func(input *SingBoxConfigInput) {
			input.SplitTunnel = &SplitTunnelRules{
				BypassCountries:  []string{"us"},
				RuleSetDirectory: "/rulesets",
			}
		},
		"duplicate country": func(input *SingBoxConfigInput) {
			input.SplitTunnel = &SplitTunnelRules{
				BypassCountries:  []string{"cn", "cn"},
				RuleSetDirectory: "/rulesets",
			}
		},
		"missing rule set directory": func(input *SingBoxConfigInput) {
			input.SplitTunnel = &SplitTunnelRules{BypassCountries: []string{"cn"}}
		},
		"bypass countries without the DoH shape": func(input *SingBoxConfigInput) {
			input.DNSShape = DNSShapeTCPProxied
			input.SplitTunnel = &SplitTunnelRules{
				BypassCountries:  []string{"cn"},
				RuleSetDirectory: "/rulesets",
			}
		},
		"excluded packages in proxy mode": func(input *SingBoxConfigInput) {
			// exclude_package is a TUN inbound field; in ModeProxy it would be
			// a silent no-op and the excluded apps' traffic would still be
			// carried through the proxy.
			input.Mode = ModeProxy
			input.ProxyListenPort = 2080
			input.SplitTunnel = &SplitTunnelRules{
				ExcludedPackages: []string{"com.tencent.mm"},
			}
		},
	} {
		input := base
		mutate(&input)
		if _, err := BuildSingBoxConfig(input); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestDoHFailoverRejectsUnrepresentableDNSServers(t *testing.T) {
	// A hostname server would be self-referential (nothing can bootstrap its
	// own resolver chain), and an unknown IP would be emitted with no
	// tls.server_name, reintroducing the IP-SAN fragility dohTLSServerNames
	// exists to prevent.
	for name, servers := range map[string][]string{
		"hostname server": {"dns.google"},
		"unpinned IP":     {"1.1.1.1", "9.9.9.9"},
	} {
		input := mobileInput(mobileRelay(t))
		input.DNSServers = servers
		if _, err := BuildSingBoxConfig(input); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		// The TCP shape's legacy emission keeps tolerating the same servers.
		input.DNSShape = DNSShapeTCPProxied
		input.ClashAPI = false
		if _, err := BuildSingBoxConfig(input); err != nil {
			t.Fatalf("%s: the TCP shape must stay tolerant: %v", name, err)
		}
	}
}

func TestTunnelDNSAddress(t *testing.T) {
	got, err := TunnelDNSAddress(DefaultTunnelIPv4Address)
	if err != nil {
		t.Fatalf("derive default tunnel DNS address: %v", err)
	}
	if got != DefaultTunnelDNSAddress {
		t.Fatalf("expected %s, got %s", DefaultTunnelDNSAddress, got)
	}
	if DefaultTunnelDNSAddress != "172.19.0.2" {
		t.Fatalf("DefaultTunnelDNSAddress drifted: %s", DefaultTunnelDNSAddress)
	}
	// Octet carry, so a future tunnel address change cannot silently derive a
	// wrong hijack address. The /23 keeps the successor inside the prefix.
	if got, err := TunnelDNSAddress("10.0.0.255/23"); err != nil || got != "10.0.1.0" {
		t.Fatalf("expected 10.0.1.0, got %s (%v)", got, err)
	}
	// sing-tun refuses to derive a hijack address whose successor escapes the
	// TUN prefix (HasNextAddress), so returning one here would fail every
	// probe on a healthy tunnel: the last address of a prefix must be
	// rejected, exactly like a non-IPv4 input.
	for _, invalid := range []string{"172.19.0.1", "fdfe:dcba:9876::1/126", "bogus/30", "10.0.0.255/30", "172.19.0.3/30"} {
		if _, err := TunnelDNSAddress(invalid); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
}

func TestMobileTUNNeverPinsItsOwnDNSAddress(t *testing.T) {
	// An explicit dns_address on the tun inbound would replace sing-tun's
	// derived hijack address and silently invalidate the probe target above.
	decoded := buildDecoded(t, mobileInput(mobileRelay(t)))
	tun := decoded["inbounds"].([]any)[0].(map[string]any)
	if _, ok := tun["dns_address"]; ok {
		t.Fatalf("tun inbound must not pin dns_address: %+v", tun)
	}
}
