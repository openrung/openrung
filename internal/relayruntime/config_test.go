package relayruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestNewXrayCommandExcludesIdentitySeedFromEnvironment(t *testing.T) {
	t.Setenv(IdentitySeedEnvironmentVariable, "long-lived-secret")
	t.Setenv("OPENRUNG_TEST_CHILD_ENV", "preserved")

	cmd := NewXrayCommand(context.Background(), "xray", "run")
	if slices.Contains(cmd.Env, IdentitySeedEnvironmentVariable+"=long-lived-secret") {
		t.Fatal("Xray command inherited OPENRUNG_IDENTITY_SEED")
	}
	if !slices.Contains(cmd.Env, "OPENRUNG_TEST_CHILD_ENV=preserved") {
		t.Fatal("Xray command dropped unrelated environment variables")
	}

	// Windows treats environment names case-insensitively. Exercise the filter
	// directly so a differently-cased spelling cannot survive into a child.
	filtered := environmentWithoutIdentitySeed([]string{
		"OpenRung_Identity_Seed=also-secret",
		"OPENRUNG_TEST_CHILD_ENV=preserved",
	})
	if len(filtered) != 1 || filtered[0] != "OPENRUNG_TEST_CHILD_ENV=preserved" {
		t.Fatalf("case-insensitive identity seed filtering = %q", filtered)
	}
}

func TestGenerateRealityKeyPairExcludesIdentitySeedFromEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Xray executable requires a POSIX shell")
	}

	fakeXray := filepath.Join(t.TempDir(), "xray")
	script := `#!/bin/sh
if [ -n "${OPENRUNG_IDENTITY_SEED:-}" ]; then
  echo "identity seed leaked to xray" >&2
  exit 97
fi
printf 'Private key: private_key\nPublic key: public-key\n'
`
	if err := os.WriteFile(fakeXray, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Xray: %v", err)
	}
	t.Setenv(IdentitySeedEnvironmentVariable, "long-lived-secret")

	keys, err := GenerateRealityKeyPair(fakeXray)
	if err != nil {
		t.Fatalf("generate Reality key pair: %v", err)
	}
	if keys.PrivateKey != "private_key" || keys.PublicKey != "public-key" {
		t.Fatalf("unexpected keys: %+v", keys)
	}
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
