package client

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenCases is the full matrix of builder inputs whose emitted JSON is frozen
// under testdata/singbox/. The first five entries are the pre-existing
// desktop/CLI combinations (TUN, TUN with an IP-literal relay, the punched TUN
// bridge, proxy mode, and the WSS loopback bridge in proxy mode); their goldens
// were generated before the mobile superset options existed and freezing them
// is what proves those options are strictly additive — any byte drift on an
// existing combination is a regression, not a formatting choice.
type goldenCase struct {
	name  string
	input SingBoxConfigInput
}

func goldenCases() []goldenCase {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	hostRelay := validRelay(now)
	ipRelay := validRelay(now)
	ipRelay.PublicHost = "203.0.113.10"

	return []goldenCase{
		{
			name:  "tun-default",
			input: SingBoxConfigInput{Relay: hostRelay},
		},
		{
			name:  "tun-ip-relay",
			input: SingBoxConfigInput{Relay: ipRelay},
		},
		{
			// The desktop punched path: outbound redirected to the loopback
			// bridge, relay endpoint and reflexive peer both excluded from the
			// TUN route (no socket protection exists outside mobile).
			name: "tun-punched",
			input: SingBoxConfigInput{
				Relay:                   ipRelay,
				BridgeHost:              "127.0.0.1",
				BridgePort:              54321,
				PunchPeerExcludeAddress: "198.51.100.7",
			},
		},
		{
			name: "proxy",
			input: SingBoxConfigInput{
				Relay:           hostRelay,
				Mode:            ModeProxy,
				ProxyListenPort: 7890,
			},
		},
		{
			// cmd/wssmatrix's WSS probe shape: a mixed loopback inbound whose
			// VLESS outbound dials the local WSS bridge.
			name: "proxy-wss-bridge",
			input: SingBoxConfigInput{
				Relay:              hostRelay,
				Mode:               ModeProxy,
				ProxyListenAddress: "127.0.0.1",
				ProxyListenPort:    7890,
				BridgeHost:         "127.0.0.1",
				BridgePort:         54321,
			},
		},

		// The mobile-shaped cases below mirror the emissions
		// SingBoxConfiguration.kt / SingBoxConfiguration.swift produce (release
		// log level, MTU 1400, DoH failover DNS, traffic accounting), so the
		// mobile-parity structural tests and these goldens together pin the
		// superset against the mobile generators' own test expectations.
		{
			// Full-device Android TUN on the direct relay path.
			name: "mobile-tun-android",
			input: SingBoxConfigInput{
				Relay:            ipRelay,
				MTU:              1400,
				LogLevel:         "warn",
				DNSShape:         DNSShapeDoHFailover,
				RouteFindProcess: true,
				ClashAPI:         true,
			},
		},
		{
			// iOS never emits find_process (nor exclude_package).
			name: "mobile-tun-ios",
			input: SingBoxConfigInput{
				Relay:    ipRelay,
				MTU:      1400,
				LogLevel: "warn",
				DNSShape: DNSShapeDoHFailover,
				ClashAPI: true,
			},
		},
		{
			// Mobile's punched AND WSS shape — both hand sing-box the same
			// loopback bridge, and the platform-protected outer socket means
			// no route_exclude_address may appear (VpnService.protect exempts
			// only the Go socket; a peer /32 would leak other apps' traffic).
			name: "mobile-bridge-android",
			input: SingBoxConfigInput{
				Relay:                 ipRelay,
				MTU:                   1400,
				LogLevel:              "warn",
				DNSShape:              DNSShapeDoHFailover,
				RouteFindProcess:      true,
				ClashAPI:              true,
				BridgeHost:            "127.0.0.1",
				BridgePort:            54321,
				BridgeOwnsOuterSocket: true,
			},
		},
		{
			// Iran split tunneling: route bypass + LAN bypass + per-app
			// exclusion, but NO dns-direct-ir server or country DNS rules —
			// Iran has no verifiable encrypted in-country resolver today.
			name: "mobile-split-ir-android",
			input: SingBoxConfigInput{
				Relay:            ipRelay,
				MTU:              1400,
				LogLevel:         "warn",
				DNSShape:         DNSShapeDoHFailover,
				RouteFindProcess: true,
				ClashAPI:         true,
				SplitTunnel: &SplitTunnelRules{
					BypassLAN:        true,
					BypassCountries:  []string{SplitTunnelCountryIR},
					ExcludedPackages: []string{"com.tencent.mm", "org.telegram.messenger"},
					RuleSetDirectory: "/data/user/0/rulesets",
				},
			},
		},
		{
			// China split tunneling on iOS: AliDNS direct resolver plus the
			// probe pins that keep geosite-cn from capturing probe lookups.
			name: "mobile-split-cn-ios",
			input: SingBoxConfigInput{
				Relay:    ipRelay,
				MTU:      1400,
				LogLevel: "warn",
				DNSShape: DNSShapeDoHFailover,
				ClashAPI: true,
				SplitTunnel: &SplitTunnelRules{
					BypassLAN:        true,
					BypassCountries:  []string{SplitTunnelCountryCN},
					RuleSetDirectory: "/var/mobile/rulesets",
				},
			},
		},
		{
			// Both countries in canonical order, matching the Kotlin
			// "both countries plus lan keep the full canonical rule order"
			// expectation.
			name: "mobile-split-ir-cn-android",
			input: SingBoxConfigInput{
				Relay:            ipRelay,
				MTU:              1400,
				LogLevel:         "warn",
				DNSShape:         DNSShapeDoHFailover,
				RouteFindProcess: true,
				ClashAPI:         true,
				SplitTunnel: &SplitTunnelRules{
					BypassLAN:        true,
					BypassCountries:  []string{SplitTunnelCountryIR, SplitTunnelCountryCN},
					RuleSetDirectory: "/data/user/0/rulesets",
				},
			},
		},
	}
}

// TestBuildSingBoxConfigGolden compares every case against its frozen file.
// Regenerate deliberately with:
//
//	UPDATE_SINGBOX_GOLDEN=1 go test ./internal/client -run TestBuildSingBoxConfigGolden
func TestBuildSingBoxConfigGolden(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildSingBoxConfig(tc.input)
			if err != nil {
				t.Fatalf("build sing-box config: %v", err)
			}
			path := filepath.Join("testdata", "singbox", tc.name+".golden.json")
			if os.Getenv("UPDATE_SINGBOX_GOLDEN") != "" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("update golden: %v", err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with UPDATE_SINGBOX_GOLDEN=1 to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("config drifted from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
			}
		})
	}
}
