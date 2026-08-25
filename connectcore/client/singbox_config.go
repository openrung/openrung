package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/openrung/openrung/brokerapi"
)

// InboundMode selects how the client captures traffic. The zero value is
// ModeTUN, so existing callers (the CLI, the mobile-serving backend) keep
// producing byte-identical full-device TUN configs.
type InboundMode int

const (
	// ModeTUN captures all device traffic via a TUN interface (needs elevated
	// privileges). This is the default and unchanged behavior.
	ModeTUN InboundMode = iota
	// ModeProxy exposes a local mixed (HTTP + SOCKS) inbound on loopback for
	// the desktop system-proxy mode, which needs no privileges. The GUI points
	// the OS proxy at ProxyListenAddress:ProxyListenPort.
	ModeProxy
)

// DNSShape selects the DNS block's emission. The zero value is
// DNSShapeTCPProxied, so existing callers keep producing byte-identical
// configs.
type DNSShape int

const (
	// DNSShapeTCPProxied is the original desktop/CLI shape: plain TCP/53
	// resolvers detoured through the proxy, final pinned to dns-0, no DNS
	// rules. Unchanged behavior.
	DNSShapeTCPProxied DNSShape = iota
	// DNSShapeDoHFailover is mobile's shape: DoH over 443 through the proxy
	// (relays answer 443 on every transport, while TCP/53 gets no replies
	// under WSS), an uncached probe-priority rule chain, per-country direct
	// resolvers for split tunneling, and a real primary-to-fallback failover
	// chain — a static final would never consult a second resolver.
	DNSShapeDoHFailover
)

const (
	// DefaultTunnelIPv4Address and DefaultTunnelIPv6Address are the TUN
	// inbound's addresses when the caller does not override them. They are
	// exported because a TUN-mode host has no loopback inbound to dial for
	// readiness and instead waits for these addresses to appear on a local
	// interface (connectcore.tunInterfaceReady).
	DefaultTunnelIPv4Address = "172.19.0.1/30"
	DefaultTunnelIPv6Address = "fdfe:dcba:9876::1/126"

	// DefaultTunnelDNSAddress is TunnelDNSAddress(DefaultTunnelIPv4Address):
	// the only in-TUN address whose port-53 traffic sing-box hijacks (see
	// TunnelDNSAddress), and therefore the address mobile's fresh-DNS probes
	// must target.
	DefaultTunnelDNSAddress = "172.19.0.2"
)

// DefaultProbeDomainSuffixes are the through-tunnel connectivity-probe
// hostnames the DoH failover shape pins through the proxy with its
// highest-priority DNS and route rules, so a country-bypass rule set that
// happens to contain a probe hostname (geosite-cn ships www.gstatic.com) can
// never route a probe onto the direct path and prove nothing about the
// tunnel. Mirrors mobile's ProbeTargets.
var DefaultProbeDomainSuffixes = []string{"probe.openrung.org", "cp.cloudflare.com"}

// dohTLSServerNames are the hostnames the DoH TLS handshakes authenticate
// while the dial stays on the IP literal, so a provider dropping IP SANs from
// its certificate cannot break resolution.
var dohTLSServerNames = map[string]string{
	"1.1.1.1": "cloudflare-dns.com",
	"8.8.8.8": "dns.google",
}

// validateDoHDNSServers rejects proxied resolvers the DoH failover emission
// cannot represent safely. A hostname server would be self-referential: its
// address could only be resolved by route.default_domain_resolver = "dns-0",
// which is the very chain it belongs to, and we emit no bootstrap resolver. An
// IP literal without a dohTLSServerNames entry would be emitted with no tls
// block, verifying the bare IP against the certificate — the IP-SAN fragility
// that map exists to prevent. Only DNSShapeDoHFailover validates: the TCP
// shape's byte-identical legacy emission keeps tolerating anything.
func validateDoHDNSServers(dnsServers []string) error {
	for _, server := range dnsServers {
		if net.ParseIP(server) == nil {
			return fmt.Errorf("DoH failover DNS server %q is not an IP literal; a hostname server has no bootstrap resolver", server)
		}
		if _, ok := dohTLSServerNames[server]; !ok {
			return fmt.Errorf("DoH failover DNS server %q has no pinned TLS server name; add it to dohTLSServerNames first", server)
		}
	}
	return nil
}

// Per-evaluate budget before the next resolver runs, and the terminal/global
// budget (DNSShapeDoHFailover only).
const (
	dnsPrimaryTimeout  = "2s"
	dnsFallbackTimeout = "3s"
)

type SingBoxConfigInput struct {
	Relay             brokerapi.RelayDescriptor
	TunnelIPv4Address string
	TunnelIPv6Address string
	DNSServers        []string
	MTU               int
	// Mode selects the inbound (TUN by default; mixed loopback for proxy mode).
	Mode InboundMode
	// ProxyListenAddress and ProxyListenPort configure the mixed inbound in
	// ModeProxy. Address defaults to 127.0.0.1; a positive port is required.
	ProxyListenAddress string
	ProxyListenPort    int
	// BridgeHost and BridgePort, when set, redirect the VLESS outbound to a local
	// punch bridge (127.0.0.1:BridgePort) instead of the relay's public endpoint.
	// The Reality identity fields are unchanged, so the end-to-end target is still
	// the real relay.
	BridgeHost string
	BridgePort int
	// PunchPeerExcludeAddress is the relay's reflexive UDP IP on the punched
	// path. It MUST be excluded from the TUN routes or the QUIC datagrams the punch
	// socket sends would be captured by sing-box's own auto_route/strict_route TUN
	// and loop back into the tunnel (deadlock). The loopback bridge address needs
	// no exclusion; this peer IP does.
	PunchPeerExcludeAddress string
	// BridgeOwnsOuterSocket marks the bridge's outer socket as protected by
	// the platform outside the TUN (Android VpnService.protect / iOS's
	// tunnel-exempt provider socket), the mobile posture. While a bridge is
	// active it suppresses every route_exclude_address: the exclusions exist
	// only to keep an unprotected transport socket out of its own TUN, and a
	// peer /32 exclusion on a protected transport would instead leak unrelated
	// apps' traffic to that IP. Without an active bridge it changes nothing.
	BridgeOwnsOuterSocket bool
	// LogLevel overrides sing-box's log level; empty keeps the original
	// "info". Mobile release builds pass "warn" — "info" logs every flow and
	// DNS query, too hot for the per-connection path inside the mobile engine.
	LogLevel string
	// DNSShape selects the DNS block (see DNSShape). The zero value keeps the
	// existing TCP-through-proxy emission byte-identical.
	DNSShape DNSShape
	// ProbeDomainSuffixes overrides DefaultProbeDomainSuffixes for the DoH
	// failover shape's probe-priority pins. Ignored by DNSShapeTCPProxied.
	ProbeDomainSuffixes []string
	// SplitTunnel enables mobile's split tunneling when non-nil: LAN bypass,
	// per-country .srs rule-set bypass with in-country direct resolvers, and
	// Android per-app exclusion. Bypass countries require DNSShapeDoHFailover
	// (see validateSplitTunnel). Nil emits a byte-identical no-split config.
	SplitTunnel *SplitTunnelRules
	// RouteFindProcess emits route.find_process, the Android emission's
	// process-matching switch; iOS and desktop leave it off.
	RouteFindProcess bool
	// ClashAPI emits an empty experimental.clash_api block. No
	// external_controller is set, so nothing listens; it just turns on
	// sing-box's traffic accounting, which feeds the cumulative
	// bytes_sent/bytes_received counters mobile reports with session telemetry.
	ClashAPI bool
}

func BuildSingBoxConfig(input SingBoxConfigInput) ([]byte, error) {
	if err := validateRelayForConfig(input.Relay); err != nil {
		return nil, err
	}
	if err := validateSplitTunnel(input); err != nil {
		return nil, err
	}

	tunnelIPv4Address := input.TunnelIPv4Address
	if tunnelIPv4Address == "" {
		tunnelIPv4Address = DefaultTunnelIPv4Address
	}
	tunnelIPv6Address := input.TunnelIPv6Address
	if tunnelIPv6Address == "" {
		tunnelIPv6Address = DefaultTunnelIPv6Address
	}
	dnsServers := input.DNSServers
	if len(dnsServers) == 0 {
		dnsServers = []string{"1.1.1.1", "8.8.8.8"}
	}
	if input.DNSShape == DNSShapeDoHFailover {
		if err := validateDoHDNSServers(dnsServers); err != nil {
			return nil, err
		}
	}
	mtu := input.MTU
	if mtu == 0 {
		mtu = 1500
	}
	if mtu < 0 {
		return nil, errors.New("mtu must be positive")
	}

	inbound, err := buildInbound(input, tunnelIPv4Address, tunnelIPv6Address, mtu)
	if err != nil {
		return nil, err
	}

	serverHost := input.Relay.PublicHost
	serverPort := input.Relay.PublicPort
	if input.BridgeHost != "" && input.BridgePort > 0 {
		serverHost = input.BridgeHost
		serverPort = input.BridgePort
	}

	logLevel := input.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}
	probeSuffixes := input.ProbeDomainSuffixes
	if len(probeSuffixes) == 0 {
		probeSuffixes = DefaultProbeDomainSuffixes
	}

	cfg := map[string]any{
		"log": map[string]any{
			"level":     logLevel,
			"timestamp": true,
		},
		"dns": buildDNSConfig(input, dnsServers, probeSuffixes),
		"inbounds": []any{
			inbound,
		},
		"outbounds": []any{
			map[string]any{
				"type":            "vless",
				"tag":             "proxy",
				"server":          serverHost,
				"server_port":     serverPort,
				"uuid":            input.Relay.ClientID,
				"flow":            input.Relay.Flow,
				"network":         "tcp",
				"packet_encoding": "xudp",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": input.Relay.ServerName,
					"utls": map[string]any{
						"enabled":     true,
						"fingerprint": "chrome",
					},
					"reality": map[string]any{
						"enabled":    true,
						"public_key": input.Relay.RealityPublicKey,
						"short_id":   input.Relay.ShortID,
					},
				},
			},
			map[string]any{
				"type": "direct",
				"tag":  "direct",
			},
			map[string]any{
				"type": "block",
				"tag":  "block",
			},
		},
		"route": buildRouteConfig(input, probeSuffixes),
	}
	if input.ClashAPI {
		// No external_controller is set, so nothing listens; an empty
		// clash_api block just turns on sing-box's traffic accounting (see
		// SingBoxConfigInput.ClashAPI).
		cfg["experimental"] = map[string]any{
			"clash_api": map[string]any{},
		}
	}

	return json.MarshalIndent(cfg, "", "  ")
}

// buildInbound constructs the single inbound for the requested mode. ModeTUN
// reproduces the original full-device TUN inbound byte-for-byte (including the
// transport-peer route exclusions); ModeProxy emits a loopback mixed inbound.
func buildInbound(input SingBoxConfigInput, tunnelIPv4Address, tunnelIPv6Address string, mtu int) (map[string]any, error) {
	if input.Mode == ModeProxy {
		listen := input.ProxyListenAddress
		if listen == "" {
			listen = "127.0.0.1"
		}
		if input.ProxyListenPort <= 0 {
			return nil, errors.New("proxy mode requires a positive ProxyListenPort")
		}
		// A mixed inbound speaks both HTTP and SOCKS on loopback; the desktop
		// proxymode controller points the OS system proxy at it. No TUN, so no
		// auto_route/strict_route and no route_exclude_address are needed — the
		// OS only sends proxy-aware traffic here, and the relay endpoint is
		// reached as ordinary direct traffic.
		return map[string]any{
			"type":        "mixed",
			"tag":         "mixed-in",
			"listen":      listen,
			"listen_port": input.ProxyListenPort,
		}, nil
	}

	tunInbound := map[string]any{
		"type":                     "tun",
		"tag":                      "tun-in",
		"address":                  []string{tunnelIPv4Address, tunnelIPv6Address},
		"mtu":                      mtu,
		"auto_route":               true,
		"strict_route":             true,
		"stack":                    "system",
		"dns_mode":                 "hijack",
		"endpoint_independent_nat": true,
	}
	if input.SplitTunnel != nil && len(input.SplitTunnel.ExcludedPackages) > 0 {
		// Excluded apps leave the VPN at the OS level (Android). NEVER emit
		// include_package alongside this: Android forbids mixing the two, and
		// we only ever exclude.
		tunInbound["exclude_package"] = input.SplitTunnel.ExcludedPackages
	}
	if input.BridgeOwnsOuterSocket && input.BridgeHost != "" && input.BridgePort > 0 {
		// A platform-protected bridge owns its outer socket, so nothing here
		// needs excluding from the TUN — and a peer /32 exclusion would leak
		// unrelated apps' traffic to that IP (mobile's leak-precedent
		// regression). The inner bridge endpoint must stay on loopback.
		return tunInbound, nil
	}
	// Exclude the real transport peers from the TUN so their traffic is not
	// captured by auto_route/strict_route. On the direct path that is the relay's
	// public IP; on the punch path it is additionally the relay's reflexive
	// UDP IP the QUIC socket talks to (see Correction #1 in the plan).
	var excludeAddresses []string
	for _, host := range []string{input.Relay.PublicHost, input.PunchPeerExcludeAddress} {
		if excludeAddress := relayRouteExcludeAddress(host); excludeAddress != "" {
			excludeAddresses = append(excludeAddresses, excludeAddress)
		}
	}
	if len(excludeAddresses) > 0 {
		tunInbound["route_exclude_address"] = excludeAddresses
	}
	return tunInbound, nil
}

func relayRouteExcludeAddress(host string) string {
	cleanHost := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(cleanHost)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return ip.String() + "/32"
	}
	return ip.String() + "/128"
}

func dnsServerObjects(servers []string) []any {
	out := make([]any, 0, len(servers))
	for i, server := range servers {
		out = append(out, map[string]any{
			"tag":    fmt.Sprintf("dns-%d", i),
			"type":   "tcp",
			"server": server,
			"detour": "proxy",
		})
	}
	return out
}

// buildDNSConfig emits the DNS block for the selected shape. The TCP shape is
// the original desktop/CLI emission, byte-identical; the DoH failover shape is
// mobile's (see DNSShapeDoHFailover).
func buildDNSConfig(input SingBoxConfigInput, dnsServers, probeSuffixes []string) map[string]any {
	if input.DNSShape != DNSShapeDoHFailover {
		return map[string]any{
			"servers": dnsServerObjects(dnsServers),
			"final":   "dns-0",
		}
	}

	var bypassCountries []string
	if input.SplitTunnel != nil {
		bypassCountries = input.SplitTunnel.BypassCountries
	}

	servers := make([]any, 0, len(dnsServers)+len(bypassCountries))
	for i, server := range dnsServers {
		// DoH over 443 via the proxy: relays answer 443 on every transport,
		// while TCP/53 gets no replies under WSS. IP-literal servers need no
		// bootstrap resolver (defaults: port 443, path /dns-query);
		// validateDoHDNSServers has already rejected anything else, and
		// guarantees the dohTLSServerNames lookup below succeeds. TLS
		// authenticates the provider hostname while the dial stays on the IP
		// literal, so a provider dropping IP SANs from its certificate cannot
		// break resolution.
		servers = append(servers, map[string]any{
			"tag":    fmt.Sprintf("dns-%d", i),
			"type":   "https",
			"server": server,
			"detour": "proxy",
			"tls": map[string]any{
				"enabled":     true,
				"server_name": dohTLSServerNames[server],
			},
		})
	}
	for _, country := range bypassCountries {
		resolver := splitTunnelDirectResolvers[country]
		if resolver == nil {
			continue
		}
		// DoH over 443 on the direct path: encrypted, and 443 survives the
		// middleboxes that a bare 853 often does not. A DNS server with no
		// detour builds its own direct dialer, which is exactly what a bypass
		// resolver needs — detouring to our otherwise-empty tagged direct
		// outbound is rejected during sing-box's Start stage.
		servers = append(servers, map[string]any{
			"tag":    "dns-direct-" + country,
			"type":   "https",
			"server": resolver.server,
			"tls": map[string]any{
				"enabled":     true,
				"server_name": resolver.tlsServerName,
			},
		})
	}

	// Highest priority: probe lookups must reach the proxied DoH resolvers
	// even when a country rule would divert them (geosite-cn contains
	// gstatic-class hosts), and must never be answered from any cache — a
	// cached answer proves nothing about the tunnel right now. The chain is
	// terminal for probe domains (its trailing route rule always fires), so a
	// future geosite refresh can never capture a probe lookup.
	rules := dnsFailoverRules(dnsServers, probeSuffixes, true)
	for _, country := range bypassCountries {
		rules = append(rules, countryDNSRules(country)...)
	}
	// Real failover for everything else: a static final would never consult a
	// second resolver. evaluate is non-terminal on a transport error, timeout,
	// SERVFAIL or REFUSED in the pinned engine — a usable answer (NOERROR, or
	// an authoritative NXDOMAIN) is returned by respond, anything else falls
	// through to the next resolver's terminal route rule.
	rules = append(rules, dnsFailoverRules(dnsServers, nil, false)...)

	return map[string]any{
		"servers": servers,
		"rules":   rules,
		"final":   fmt.Sprintf("dns-%d", len(dnsServers)-1),
		"timeout": dnsFallbackTimeout,
	}
}

// dnsFailoverRules emits an ordered primary-to-fallback resolver chain from
// sing-box 1.14 DNS rule actions: for every resolver but the last, evaluate
// exchanges the query (non-terminal on error) and respond returns its answer
// when the RCODE is NOERROR or NXDOMAIN — both are real answers, and probe
// nonce queries are expected to draw NXDOMAIN. Everything else (timeout,
// transport error, SERVFAIL, REFUSED) falls through until the last resolver's
// terminal route rule. With domainSuffixes the whole chain applies only to
// those domains and is guaranteed terminal for them; with nil it applies to
// every remaining query.
func dnsFailoverRules(dnsServers, domainSuffixes []string, disableCache bool) []any {
	scopeAndCache := func(rule map[string]any) map[string]any {
		if len(domainSuffixes) > 0 {
			rule["domain_suffix"] = domainSuffixes
		}
		if disableCache {
			rule["disable_cache"] = true
			rule["disable_optimistic_cache"] = true
		}
		return rule
	}
	var rules []any
	for i := range dnsServers[:len(dnsServers)-1] {
		rules = append(rules, scopeAndCache(map[string]any{
			"action":  "evaluate",
			"server":  fmt.Sprintf("dns-%d", i),
			"timeout": dnsPrimaryTimeout,
		}))
		for _, rcode := range []string{"NOERROR", "NXDOMAIN"} {
			respond := map[string]any{
				"match_response": true,
				"response_rcode": rcode,
				"action":         "respond",
			}
			if len(domainSuffixes) > 0 {
				respond["domain_suffix"] = domainSuffixes
			}
			rules = append(rules, respond)
		}
	}
	return append(rules, scopeAndCache(map[string]any{
		"server":  fmt.Sprintf("dns-%d", len(dnsServers)-1),
		"timeout": dnsFallbackTimeout,
	}))
}

// countryDNSRules is the per-country lookup chain for the domains that country
// bypasses: ask the in-country DoH resolver, and return its answer when it is
// a real one (NOERROR, or an authoritative NXDOMAIN). Nothing else is
// terminal, so a resolver that is unreachable, times out, or has let its
// certificate lapse falls through to the proxied global chain below instead of
// failing the lookup outright — the same fail-open posture as the rest of the
// feature, and the reason moving off plaintext UDP cannot cost anyone
// reachability.
//
// rule_set matches against the queried domain in both the query and the
// response pass, so the response rules stay scoped to this country's domains.
//
// Empty for a country with no usable in-country resolver: with nothing to
// evaluate, its lookups simply reach the global chain, which is where they
// would end up anyway.
func countryDNSRules(country string) []any {
	if splitTunnelDirectResolvers[country] == nil {
		return nil
	}
	ruleSet := []string{"geosite-" + country}
	rules := []any{
		map[string]any{
			"rule_set": ruleSet,
			"action":   "evaluate",
			"server":   "dns-direct-" + country,
			"timeout":  dnsPrimaryTimeout,
		},
	}
	for _, rcode := range []string{"NOERROR", "NXDOMAIN"} {
		rules = append(rules, map[string]any{
			"rule_set":       ruleSet,
			"match_response": true,
			"response_rcode": rcode,
			"action":         "respond",
		})
	}
	return rules
}

// buildRouteConfig emits the route block. Without split tunneling it is the
// original emission (a single hijack-dns rule and final proxy), byte-identical
// for existing callers.
func buildRouteConfig(input SingBoxConfigInput, probeSuffixes []string) map[string]any {
	var bypassCountries []string
	bypassLAN := false
	if input.SplitTunnel != nil {
		bypassCountries = input.SplitTunnel.BypassCountries
		bypassLAN = input.SplitTunnel.BypassLAN
	}

	rules := []any{
		map[string]any{
			"protocol": "dns",
			"action":   "hijack-dns",
		},
	}
	if len(bypassCountries) > 0 {
		// Route rules need a sniffed domain before geosite matching can work.
		rules = append(rules, map[string]any{
			"action": "sniff",
		})
		// Probe traffic must reach the proxy even when a bypass rule would
		// send it direct; a probe that escapes onto the direct path can report
		// CONNECTED over a dead tunnel. Must precede every bypass rule.
		// Scoped to TCP — probes are HTTP over TCP, and an unscoped pin would
		// swallow QUIC to these domains here, ahead of the udp-443 reject.
		rules = append(rules, map[string]any{
			"network":       "tcp",
			"domain_suffix": probeSuffixes,
			"outbound":      "proxy",
		})
	}
	if bypassLAN {
		rules = append(rules, map[string]any{
			"ip_is_private": true,
			"outbound":      "direct",
		})
	}
	for _, country := range bypassCountries {
		rules = append(rules, map[string]any{
			"rule_set": []string{"geosite-" + country, "geoip-" + country},
			"outbound": "direct",
		})
	}
	// The proxy outbound is network: tcp, so QUIC datagrams already die — but
	// silently, leaving browsers to blackhole-and-retry before falling back to
	// TCP. Rejecting UDP 443 explicitly makes that fallback immediate. Must
	// stay after every rule above: split-tunneled UDP 443 still goes direct,
	// and hijacked DNS is never at risk. no_drop, because without it sing-box
	// downgrades reject to a silent drop after 50 triggers in 30s — exactly
	// the blackhole this rule exists to remove.
	rules = append(rules, map[string]any{
		"network": "udp",
		"port":    443,
		"action":  "reject",
		"no_drop": true,
	})

	route := map[string]any{
		"auto_detect_interface":   true,
		"default_domain_resolver": "dns-0",
		"rules":                   rules,
		"final":                   "proxy",
	}
	if input.RouteFindProcess {
		route["find_process"] = true
	}
	if len(bypassCountries) > 0 {
		ruleSets := make([]any, 0, 2*len(bypassCountries))
		for _, country := range bypassCountries {
			ruleSets = append(ruleSets,
				localRuleSetObject(input.SplitTunnel.RuleSetDirectory, "geosite-"+country),
				localRuleSetObject(input.SplitTunnel.RuleSetDirectory, "geoip-"+country))
		}
		route["rule_set"] = ruleSets
	}
	return route
}

func localRuleSetObject(directory, tag string) map[string]any {
	return map[string]any{
		"type":   "local",
		"tag":    tag,
		"format": "binary",
		"path":   directory + "/" + tag + ".srs",
	}
}

// TunnelDNSAddress returns the next IPv4 address after tunnelIPv4Address,
// mirroring sing-tun's derivation of the DNS hijack address. When the tun
// inbound carries no explicit dns_address (we emit none), sing-tun derives the
// hijack address as the next address after the TUN's own IPv4 address, and the
// tun inbound tags a packet Protocol=DNS only when its destination equals that
// address — after which the router hijacks it into the DNS module ahead of any
// route rule. A datagram addressed to a public resolver (1.1.1.1) is NOT
// tagged, matches no rule, and dies on the TCP-only proxy outbound, so
// mobile's fresh-DNS probe must target this address.
//
// sing-tun only performs that derivation when the successor stays inside the
// TUN prefix (HasNextAddress: prefix.Contains(addr.Next())); otherwise it
// hijacks no IPv4 address at all. A tunnel address whose successor escapes the
// prefix is therefore rejected here — returning it would hand probes an
// address sing-box never hijacks and fail them on a healthy tunnel.
func TunnelDNSAddress(tunnelIPv4Address string) (string, error) {
	prefix, err := netip.ParsePrefix(tunnelIPv4Address)
	if err != nil || !prefix.Addr().Is4() {
		return "", fmt.Errorf("tunnel address is not an IPv4 prefix: %s", tunnelIPv4Address)
	}
	next := prefix.Addr().Next()
	if !prefix.Contains(next) {
		return "", fmt.Errorf("tunnel address %s has no successor inside its prefix for sing-tun to hijack", tunnelIPv4Address)
	}
	return next.String(), nil
}

func validateRelayForConfig(candidate brokerapi.RelayDescriptor) error {
	if candidate.Protocol != brokerapi.ProtocolVLESSRealityVision {
		return errors.New("relay protocol is not vless-reality-vision")
	}
	if candidate.Flow != brokerapi.FlowVision {
		return errors.New("relay flow is not xtls-rprx-vision")
	}
	if candidate.ExitMode != brokerapi.ExitModeDirect {
		return errors.New("relay exit mode is not direct")
	}
	if !hasRequiredConnectionFields(candidate) {
		return errors.New("relay is missing required connection fields")
	}
	return nil
}
