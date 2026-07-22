package main

import (
	"os"
	"path/filepath"
	"testing"

	"openrung/internal/wssbridge"
)

func TestKeygenAndOverlappingKeyring(t *testing.T) {
	temp := t.TempDir()
	seedA := filepath.Join(temp, "a.seed")
	publicA := filepath.Join(temp, "a.pub")
	seedB := filepath.Join(temp, "b.seed")
	publicB := filepath.Join(temp, "b.pub")
	for _, pair := range [][2]string{{seedA, publicA}, {seedB, publicB}} {
		if err := keygen([]string{"-seed-file", pair[0], "-public-key-file", pair[1]}); err != nil {
			t.Fatal(err)
		}
		for _, path := range pair {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("%s mode = %o", path, got)
			}
		}
	}
	ring := filepath.Join(temp, "ring")
	if err := keyring([]string{
		"-output", ring,
		"-public-key-file", publicA,
		"-public-key-file", publicB,
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := readMode0600Line(ring)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := wssbridge.ParseTicketPublicKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 {
		t.Fatalf("key ring size = %d", len(parsed))
	}
	if err := keygen([]string{"-seed-file", seedA, "-public-key-file", publicA}); err == nil {
		t.Fatal("keygen replaced existing key files")
	}
}

func TestParseConfigSeparatesEdgeAndOriginInputs(t *testing.T) {
	edge, err := parseConfig([]string{
		"-mode", "edge", "-url", "wss://example.cloudfront.net/api/v1/wss-bridge",
		"-relay-id", "relay_a", "-front-id", "front-a", "-seed-file", "/tmp/seed",
		"-descriptor-file", "/tmp/descriptor",
	})
	if err != nil || edge.mode != "edge" {
		t.Fatalf("edge config: %#v, %v", edge, err)
	}
	origin, err := parseConfig([]string{
		"-mode", "origin", "-url", "ws://127.0.0.1:8081/api/v1/wss-bridge",
		"-relay-id", "relay_a", "-front-id", "front-a", "-seed-file", "/tmp/seed",
		"-origin-token-file", "/tmp/current", "-origin-token-next-file", "/tmp/next",
		"-source-limit", "2", "-expect-close-within", "10s",
	})
	if err != nil || origin.mode != "origin" {
		t.Fatalf("origin config: %#v, %v", origin, err)
	}
	if _, err := parseConfig([]string{
		"-mode", "origin", "-url", "ws://relay.example/api/v1/wss-bridge",
		"-relay-id", "relay_a", "-front-id", "front-a", "-seed-file", "/tmp/seed",
		"-origin-token-file", "/tmp/current", "-origin-token-next-file", "/tmp/next",
		"-source-limit", "2",
	}); err == nil {
		t.Fatal("origin mode accepted a non-loopback endpoint")
	}
	revoked, err := parseConfig([]string{
		"-mode", "revoked", "-url", "wss://example.cloudfront.net/api/v1/wss-bridge",
		"-relay-id", "relay_a", "-front-id", "front-a", "-seed-file", "/tmp/seed",
	})
	if err != nil || revoked.mode != "revoked" {
		t.Fatalf("revoked config: %#v, %v", revoked, err)
	}
	issued, err := parseConfig([]string{
		"-mode", "issued", "-url", "wss://example.cloudfront.net/api/v1/wss-bridge",
		"-relay-id", "relay_a", "-front-id", "front-a",
		"-ticket-response-file", "/tmp/ticket-response", "-descriptor-file", "/tmp/descriptor",
	})
	if err != nil || issued.mode != "issued" {
		t.Fatalf("issued config: %#v, %v", issued, err)
	}
}
