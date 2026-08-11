package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"openrung/internal/relay"
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

type SingBoxConfigInput struct {
	Relay             relay.Descriptor
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
	// SplitTunnel contains validated proxy-mode bypass rules. A nil or inert
	// value preserves the baseline config byte-for-byte. Desktop intentionally
	// has no per-process exclusions: only requests entering the mixed proxy can
	// be routed here.
	SplitTunnel *SplitTunnelRules
}

// SplitTunnelRules is the ready-to-emit form of the desktop split-tunnel
// preference. Callers validate/stage both binary rule-set files for every
// country before constructing it. Countries are normalized again here as a
// final forward-compatible guard.
type SplitTunnelRules struct {
	BypassLAN           bool
	BypassCountries     []string
	RuleSetDirectory    string
	ProxyDomainSuffixes []string
}

type splitTunnelCountry struct {
	code           string
	geositeTag     string
	geoipTag       string
	directResolver string
}

var supportedSplitTunnelCountries = []splitTunnelCountry{
	{code: "ir", geositeTag: "geosite-ir", geoipTag: "geoip-ir", directResolver: "178.22.122.100"},
	{code: "cn", geositeTag: "geosite-cn", geoipTag: "geoip-cn", directResolver: "223.5.5.5"},
}

var defaultProxyProbeDomainSuffixes = []string{"www.gstatic.com", "cp.cloudflare.com"}

func BuildSingBoxConfig(input SingBoxConfigInput) ([]byte, error) {
	if err := validateRelayForConfig(input.Relay); err != nil {
		return nil, err
	}

	tunnelIPv4Address := input.TunnelIPv4Address
	if tunnelIPv4Address == "" {
		tunnelIPv4Address = "172.19.0.1/30"
	}
	tunnelIPv6Address := input.TunnelIPv6Address
	if tunnelIPv6Address == "" {
		tunnelIPv6Address = "fdfe:dcba:9876::1/126"
	}
	dnsServers := input.DNSServers
	if len(dnsServers) == 0 {
		dnsServers = []string{"1.1.1.1", "8.8.8.8"}
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

	dns := map[string]any{
		"servers": dnsServerObjects(dnsServers),
		"final":   "dns-0",
	}
	route := map[string]any{
		"auto_detect_interface":   true,
		"default_domain_resolver": "dns-0",
		"rules": []any{
			map[string]any{
				"protocol": "dns",
				"action":   "hijack-dns",
			},
		},
		"final": "proxy",
	}
	applySplitTunnelConfig(dns, route, input.SplitTunnel)

	cfg := map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"dns": dns,
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
		"route": route,
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func applySplitTunnelConfig(dns, route map[string]any, rules *SplitTunnelRules) {
	if rules == nil {
		return
	}
	countries := normalizedSplitTunnelCountries(rules.BypassCountries, rules.RuleSetDirectory)
	if !rules.BypassLAN && len(countries) == 0 {
		return
	}

	routeRules := route["rules"].([]any)
	if len(countries) > 0 {
		probeSuffixes := normalizedDomainSuffixes(rules.ProxyDomainSuffixes)
		if len(probeSuffixes) == 0 {
			probeSuffixes = append([]string(nil), defaultProxyProbeDomainSuffixes...)
		}
		routeRules = append(routeRules,
			map[string]any{"action": "sniff"},
			map[string]any{"domain_suffix": probeSuffixes, "outbound": "proxy"},
		)
	}
	if rules.BypassLAN || len(countries) > 0 {
		// Mixed HTTP/SOCKS requests commonly arrive with a hostname rather than
		// an IP destination. Resolve it before IP-based LAN/geoip matching so the
		// proxy-only desktop port preserves the mobile client's effective rules.
		// The force-proxy probe rule above remains terminal and therefore cannot
		// be diverted by an address learned here.
		routeRules = append(routeRules, map[string]any{"action": "resolve"})
	}
	if rules.BypassLAN {
		routeRules = append(routeRules, map[string]any{
			"ip_is_private": true,
			"outbound":      "direct",
		})
	}
	for _, country := range countries {
		routeRules = append(routeRules, map[string]any{
			"rule_set": []string{country.geositeTag, country.geoipTag},
			"outbound": "direct",
		})
	}
	route["rules"] = routeRules

	if len(countries) == 0 {
		return
	}

	ruleSets := make([]any, 0, len(countries)*2)
	for _, country := range countries {
		for _, tag := range []string{country.geositeTag, country.geoipTag} {
			ruleSets = append(ruleSets, map[string]any{
				"type":   "local",
				"tag":    tag,
				"format": "binary",
				"path":   filepath.Join(rules.RuleSetDirectory, tag+".srs"),
			})
		}
	}
	route["rule_set"] = ruleSets

	dnsServers := dns["servers"].([]any)
	for _, country := range countries {
		dnsServers = append(dnsServers, map[string]any{
			"tag":    "dns-direct-" + country.code,
			"type":   "udp",
			"server": country.directResolver,
		})
	}
	dns["servers"] = dnsServers

	probeSuffixes := normalizedDomainSuffixes(rules.ProxyDomainSuffixes)
	if len(probeSuffixes) == 0 {
		probeSuffixes = append([]string(nil), defaultProxyProbeDomainSuffixes...)
	}
	dnsRules := []any{
		map[string]any{"domain_suffix": probeSuffixes, "server": "dns-0"},
	}
	for _, country := range countries {
		dnsRules = append(dnsRules, map[string]any{
			"rule_set": []string{country.geositeTag},
			"server":   "dns-direct-" + country.code,
		})
	}
	dns["rules"] = dnsRules
}

func normalizedSplitTunnelCountries(codes []string, ruleSetDirectory string) []splitTunnelCountry {
	if strings.TrimSpace(ruleSetDirectory) == "" {
		return nil
	}
	requested := make(map[string]bool, len(codes))
	for _, code := range codes {
		requested[strings.ToLower(strings.TrimSpace(code))] = true
	}
	out := make([]splitTunnelCountry, 0, len(supportedSplitTunnelCountries))
	for _, country := range supportedSplitTunnelCountries {
		if requested[country.code] {
			out = append(out, country)
		}
	}
	return out
}

func normalizedDomainSuffixes(suffixes []string) []string {
	seen := make(map[string]bool, len(suffixes))
	out := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		suffix = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(suffix)), ".")
		if suffix == "" || seen[suffix] {
			continue
		}
		seen[suffix] = true
		out = append(out, suffix)
	}
	return out
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

func validateRelayForConfig(candidate relay.Descriptor) error {
	if candidate.Protocol != relay.ProtocolVLESSRealityVision {
		return errors.New("relay protocol is not vless-reality-vision")
	}
	if candidate.Flow != relay.FlowVision {
		return errors.New("relay flow is not xtls-rprx-vision")
	}
	if candidate.ExitMode != relay.ExitModeDirect {
		return errors.New("relay exit mode is not direct")
	}
	if !hasRequiredConnectionFields(candidate) {
		return errors.New("relay is missing required connection fields")
	}
	return nil
}
