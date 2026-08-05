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
