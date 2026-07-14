package relayruntime

import (
	"encoding/json"
	"testing"
)

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
