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

// With APIPort set the config gains xray's management layout — the api
// object with HandlerService only, a loopback dokodemo inbound and the
// routing rule to it — and may omit the static client entirely, leaving an
// empty (not null) client list for runtime credentials. Without it the config
// carries no API surface.
func TestBuildXrayConfigWithManagementAPI(t *testing.T) {
	base := XrayConfigInput{
		ListenHost:        "127.0.0.1",
		ListenPort:        8443,
		Flow:              "xtls-rprx-vision",
		Dest:              "www.cloudflare.com:443",
		ServerName:        "www.cloudflare.com",
		RealityPrivateKey: "private",
		ShortID:           "0123456789abcdef",
	}
	if _, err := BuildXrayConfig(base); err == nil {
		t.Fatal("no client ID and no API port must be rejected")
	}
	withAPI := base
	withAPI.APIPort = 10085
	raw, err := BuildXrayConfig(withAPI)
	if err != nil {
		t.Fatalf("build with API: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	for _, key := range []string{"api", "routing"} {
		if _, ok := cfg[key]; !ok {
			t.Fatalf("config lacks %q: %s", key, raw)
		}
	}
	for _, key := range []string{"stats", "policy"} {
		if _, ok := cfg[key]; ok {
			t.Fatalf("config carries %q, which nothing reads: %s", key, raw)
		}
	}
	if services := cfg["api"].(map[string]any)["services"].([]any); len(services) != 1 || services[0] != "HandlerService" {
		t.Fatalf("api services = %v, want HandlerService only", services)
	}
	assertEgressGuard(t, cfg, 10085)
	inbounds := cfg["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Fatalf("inbounds = %d, want the relay inbound plus the API inbound", len(inbounds))
	}
	relayInbound := inbounds[0].(map[string]any)
	clients, ok := relayInbound["settings"].(map[string]any)["clients"].([]any)
	if !ok || len(clients) != 0 {
		t.Fatalf("clients = %v, want an empty list", relayInbound["settings"].(map[string]any)["clients"])
	}
	apiInbound := inbounds[1].(map[string]any)
	if apiInbound["tag"] != "api-in" || apiInbound["listen"] != "127.0.0.1" || apiInbound["port"] != float64(10085) || apiInbound["protocol"] != "dokodemo-door" {
		t.Fatalf("api inbound = %v", apiInbound)
	}
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	rule := rules[len(rules)-1].(map[string]any)
	if rule["outboundTag"] != "api" || rule["inboundTag"].([]any)[0] != "api-in" {
		t.Fatalf("api routing rule = %v", rule)
	}

	withBoth := withAPI
	withBoth.ClientID = "b831381d-6324-4d53-ad4f-8cda48b30811"
	raw, err = BuildXrayConfig(withBoth)
	if err != nil {
		t.Fatalf("build with API and static client: %v", err)
	}
	if !strings.Contains(string(raw), `"id": "b831381d-6324-4d53-ad4f-8cda48b30811"`) {
		t.Fatalf("static client dropped when the API is enabled: %s", raw)
	}

	static := base
	static.ClientID = "b831381d-6324-4d53-ad4f-8cda48b30811"
	raw, err = BuildXrayConfig(static)
	if err != nil {
		t.Fatalf("build static: %v", err)
	}
	for _, key := range []string{`"api"`, `"stats"`, `"policy"`, `"api-in"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("static config must not carry %s: %s", key, raw)
		}
	}
	var staticCfg map[string]any
	if err := json.Unmarshal(raw, &staticCfg); err != nil {
		t.Fatalf("static config is not JSON: %v", err)
	}
	assertEgressGuard(t, staticCfg, 0)
	withAPI.APIPort = withAPI.ListenPort
	if _, err := BuildXrayConfig(withAPI); err == nil {
		t.Fatal("an API port equal to the listen port must be rejected")
	}
}

// assertEgressGuard checks the routing that keeps a client from dialing the
// relay host itself: domains resolved for IP matching, the loopback/private
// IP rule, and — when the management API is on — the port rule that refuses
// the API port on every address.
func assertEgressGuard(t *testing.T, cfg map[string]any, apiPort int) {
	t.Helper()
	routing, ok := cfg["routing"].(map[string]any)
	if !ok {
		t.Fatalf("config has no routing: %v", cfg)
	}
	if routing["domainStrategy"] != "IPIfNonMatch" {
		t.Fatalf("routing domainStrategy = %v, want IPIfNonMatch so domain destinations meet the IP rule", routing["domainStrategy"])
	}
	rules := routing["rules"].([]any)
	ipRule := rules[0].(map[string]any)
	if ipRule["inboundTag"].([]any)[0] != XrayInboundTag || ipRule["outboundTag"] != xrayBlockOutboundTag {
		t.Fatalf("first rule must block the relay inbound's private egress: %v", ipRule)
	}
	cidrs := ipRule["ip"].([]any)
	for _, want := range []string{"127.0.0.0/8", "::1/128", "0.0.0.0/8", "169.254.0.0/16", "10.0.0.0/8", "fc00::/7"} {
		found := false
		for _, got := range cidrs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("egress guard lacks %s: %v", want, cidrs)
		}
	}
	outbounds := cfg["outbounds"].([]any)
	if len(outbounds) != 2 || outbounds[1].(map[string]any)["tag"] != xrayBlockOutboundTag || outbounds[1].(map[string]any)["protocol"] != "blackhole" {
		t.Fatalf("outbounds = %v, want direct + a blackhole tagged %q", outbounds, xrayBlockOutboundTag)
	}
	if apiPort == 0 {
		if len(rules) != 1 {
			t.Fatalf("static config has %d routing rules, want the IP guard only: %v", len(rules), rules)
		}
		return
	}
	portRule := rules[1].(map[string]any)
	if portRule["inboundTag"].([]any)[0] != XrayInboundTag || portRule["port"] != float64(apiPort) || portRule["outboundTag"] != xrayBlockOutboundTag {
		t.Fatalf("second rule must refuse the API port %d from the relay inbound: %v", apiPort, portRule)
	}
}
