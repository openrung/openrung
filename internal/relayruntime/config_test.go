package relayruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNewXrayCommandSanitizesOpenRungEnvironment(t *testing.T) {
	secretVariables := []string{
		"OPENRUNG_VOLUNTEER_TOKEN",
		"openrung_foundation_token",
		"OpenRung_Relay_Signing_Key",
		"OPENRUNG_DASHBOARD_TOKEN",
		"openrung_api_token",
		"OpenRung_Identity_Seed",
		"OPENRUNG_REALITY_private_KEY",
	}
	for _, name := range secretVariables {
		t.Setenv(name, "must-not-reach-xray")
	}
	t.Setenv("XRAY_LOCATION_ASSET", "/xray-assets")

	cmd := NewXrayCommand(context.Background(), "xray", "run")
	for _, name := range secretVariables {
		if environmentContainsVariable(cmd.Env, name) {
			t.Fatalf("Xray command inherited %s", name)
		}
	}
	if !slices.Contains(cmd.Env, "XRAY_LOCATION_ASSET=/xray-assets") {
		t.Fatal("Xray command dropped unrelated environment variables")
	}
}

func TestGenerateRealityKeyPairSanitizesOpenRungEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Xray executable requires a POSIX shell")
	}

	fakeXray := filepath.Join(t.TempDir(), "xray")
	script := `#!/bin/sh
if env | grep -Eiq '^openrung_'; then
  echo "OpenRung environment leaked to xray" >&2
  exit 97
fi
if [ "${XRAY_LOCATION_ASSET:-}" != "/xray-assets" ]; then
  echo "Xray runtime environment was dropped" >&2
  exit 98
fi
printf 'Private key: private_key\nPublic key: public-key\n'
`
	if err := os.WriteFile(fakeXray, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Xray: %v", err)
	}
	for _, name := range []string{
		"OpenRung_Volunteer_Token",
		"OPENRUNG_FOUNDATION_TOKEN",
		"openrung_identity_seed",
		"OPENRUNG_Reality_Private_Key",
	} {
		t.Setenv(name, "must-not-reach-xray")
	}
	t.Setenv("XRAY_LOCATION_ASSET", "/xray-assets")

	keys, err := GenerateRealityKeyPair(fakeXray)
	if err != nil {
		t.Fatalf("generate Reality key pair: %v", err)
	}
	if keys.PrivateKey != "private_key" || keys.PublicKey != "public-key" {
		t.Fatalf("unexpected keys: %+v", keys)
	}
}

func environmentContainsVariable(environ []string, want string) bool {
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

func TestBuildXrayConfig(t *testing.T) {
	cfg, err := BuildXrayConfig(XrayConfigInput{
		ListenPort:        443,
		ClientID:          "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
		Flow:              "xtls-rprx-vision",
		Dest:              "www.cloudflare.com:443",
		ServerName:        "www.cloudflare.com",
		RealityPrivateKey: "private-key",
		ShortID:           "5f7a8d9c01ab23cd",
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}

	inbounds := decoded["inbounds"].([]any)
	inbound := inbounds[0].(map[string]any)
	if inbound["listen"] != "::" {
		t.Fatalf("expected default listen host ::, got %v", inbound["listen"])
	}
}

func TestParseRealityKeyPair(t *testing.T) {
	keyPair, err := ParseRealityKeyPair([]byte("Private key: abc_123\nPublic key: def-456\n"))
	if err != nil {
		t.Fatalf("parse key pair: %v", err)
	}
	if keyPair.PrivateKey != "abc_123" || keyPair.PublicKey != "def-456" {
		t.Fatalf("unexpected key pair: %+v", keyPair)
	}
}

func TestParseRealityKeyPairCurrentXrayOutput(t *testing.T) {
	keyPair, err := ParseRealityKeyPair([]byte("PrivateKey: abc_123\nPassword (PublicKey): def-456\nHash32: ignored\n"))
	if err != nil {
		t.Fatalf("parse key pair: %v", err)
	}
	if keyPair.PrivateKey != "abc_123" || keyPair.PublicKey != "def-456" {
		t.Fatalf("unexpected key pair: %+v", keyPair)
	}
}

func TestGenerateUUID(t *testing.T) {
	id, err := GenerateUUID()
	if err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	if len(id) != 36 {
		t.Fatalf("expected UUID length 36, got %d: %q", len(id), id)
	}
}

// buildTestXrayConfig returns a decoded config from valid input.
func buildTestXrayConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := BuildXrayConfig(XrayConfigInput{
		ListenPort:        443,
		ClientID:          "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
		Flow:              "xtls-rprx-vision",
		Dest:              "www.cloudflare.com:443",
		ServerName:        "www.cloudflare.com",
		RealityPrivateKey: "private-key",
		ShortID:           "5f7a8d9c01ab23cd",
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	return decoded
}

// routingRuleFor returns the first routing rule carrying the given key.
func routingRuleFor(t *testing.T, cfg map[string]any, key string) map[string]any {
	t.Helper()
	routing, ok := cfg["routing"].(map[string]any)
	if !ok {
		t.Fatal("config has no routing section: relay egress is unrestricted")
	}
	rules, ok := routing["rules"].([]any)
	if !ok {
		t.Fatal("routing section has no rules")
	}
	for _, entry := range rules {
		rule, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if _, present := rule[key]; present {
			return rule
		}
	}
	t.Fatalf("no routing rule matches on %q", key)
	return nil
}

func TestBuildXrayConfigBlocksPrivateDestinations(t *testing.T) {
	cfg := buildTestXrayConfig(t)
	rule := routingRuleFor(t, cfg, "ip")

	if rule["outboundTag"] != blockOutboundTag {
		t.Fatalf("private-IP rule routes to %v, want %q", rule["outboundTag"], blockOutboundTag)
	}

	got := map[string]bool{}
	for _, entry := range rule["ip"].([]any) {
		got[entry.(string)] = true
	}
	// 169.254.0.0/16 carries the cloud metadata endpoint (169.254.169.254)
	// that serves instance credentials; 127.0.0.0/8 reaches the relay itself.
	for _, want := range []string{"127.0.0.0/8", "169.254.0.0/16", "10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12", "::1/128", "fc00::/7"} {
		if !got[want] {
			t.Errorf("private-IP rule is missing %s", want)
		}
	}
}

func TestBuildXrayConfigBlocksBitTorrent(t *testing.T) {
	cfg := buildTestXrayConfig(t)
	rule := routingRuleFor(t, cfg, "protocol")

	if rule["outboundTag"] != blockOutboundTag {
		t.Fatalf("bittorrent rule routes to %v, want %q", rule["outboundTag"], blockOutboundTag)
	}
	protocols, ok := rule["protocol"].([]any)
	if !ok || len(protocols) == 0 || protocols[0] != "bittorrent" {
		t.Fatalf("expected a bittorrent protocol rule, got %v", rule["protocol"])
	}
}

// The bittorrent rule matches a sniffed protocol, so disabling sniffing would
// silently stop blocking torrents while leaving the rule in place.
func TestBuildXrayConfigKeepsSniffingEnabledForProtocolRouting(t *testing.T) {
	cfg := buildTestXrayConfig(t)
	inbound := cfg["inbounds"].([]any)[0].(map[string]any)
	sniffing, ok := inbound["sniffing"].(map[string]any)
	if !ok {
		t.Fatal("inbound lost its sniffing block; the bittorrent rule cannot match without it")
	}
	if sniffing["enabled"] != true {
		t.Fatal("sniffing disabled; the bittorrent routing rule would never match")
	}
}

func TestBuildXrayConfigKeepsDirectOutboundDefault(t *testing.T) {
	cfg := buildTestXrayConfig(t)
	outbounds := cfg["outbounds"].([]any)
	if len(outbounds) < 2 {
		t.Fatalf("expected direct and block outbounds, got %d", len(outbounds))
	}
	// Xray sends traffic no rule matches to the FIRST outbound, so "direct"
	// leading is what keeps ordinary browsing working.
	first := outbounds[0].(map[string]any)
	if first["tag"] != directOutboundTag || first["protocol"] != "freedom" {
		t.Fatalf("first outbound must be the direct freedom outbound, got %v", first)
	}
	second := outbounds[1].(map[string]any)
	if second["tag"] != blockOutboundTag || second["protocol"] != "blackhole" {
		t.Fatalf("second outbound must be the block blackhole outbound, got %v", second)
	}
}

// geoip.dat ships only in the relay container image. Desktop volunteer hosts
// resolve a bare xray binary that may have no assets beside it, and Xray
// refuses to start on a geoip reference it cannot load, so the shared config
// must stay asset-free.
func TestBuildXrayConfigAvoidsGeoAssetReferences(t *testing.T) {
	raw, err := BuildXrayConfig(XrayConfigInput{
		ListenPort:        443,
		ClientID:          "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
		Flow:              "xtls-rprx-vision",
		Dest:              "www.cloudflare.com:443",
		ServerName:        "www.cloudflare.com",
		RealityPrivateKey: "private-key",
		ShortID:           "5f7a8d9c01ab23cd",
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	for _, forbidden := range []string{"geoip:", "geosite:", "ext:"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("config references %q, which needs asset files the desktop volunteer host may not have", forbidden)
		}
	}
}
