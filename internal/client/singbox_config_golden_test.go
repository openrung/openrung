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
