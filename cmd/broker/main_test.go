package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	// Injection and fallback resolution are internal/buildinfo's tests; this
	// guards the broker's wiring: its component name and embedded VERSION.
	want := "broker/" + strings.TrimSpace(baseVersion) + " revision="
	if got := versionInfo(); !strings.HasPrefix(got, want) {
		t.Fatalf("versionInfo() = %q, want prefix %q", got, want)
	}
}

func TestParseOptionalWSSTicketSeed(t *testing.T) {
	if seed, err := parseOptionalWSSTicketSeed(""); err != nil || seed != nil {
		t.Fatalf("empty seed = %x, %v", seed, err)
	}
	raw := []byte(strings.Repeat("t", 32))
	seed, err := parseOptionalWSSTicketSeed(base64.StdEncoding.EncodeToString(raw))
	if err != nil || string(seed) != string(raw) {
		t.Fatalf("parsed seed = %x, %v", seed, err)
	}
	for _, value := range []string{"not-base64", base64.StdEncoding.EncodeToString(raw[:31])} {
		if _, err := parseOptionalWSSTicketSeed(value); err == nil {
			t.Fatalf("accepted invalid seed %q", value)
		}
	}
}

func TestValidateInventoryToken(t *testing.T) {
	if err := validateInventoryToken("", "volunteer", "foundation", "dashboard", "relay-seed", "wss-seed"); err != nil {
		t.Fatalf("empty inventory token: %v", err)
	}
	for name, values := range map[string]struct {
		inventory, registration, foundation, dashboard, relaySigningKey, wssTicketSigningSeed string
	}{
		"volunteer":               {"same", "same", "foundation", "dashboard", "relay-seed", "wss-seed"},
		"foundation":              {"same", "volunteer", "same", "dashboard", "relay-seed", "wss-seed"},
		"dashboard":               {"same", "volunteer", "foundation", "same", "relay-seed", "wss-seed"},
		"relay signing seed":      {"same", "volunteer", "foundation", "dashboard", "same", "wss-seed"},
		"WSS ticket signing seed": {"same", "volunteer", "foundation", "dashboard", "relay-seed", "same"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInventoryToken(values.inventory, values.registration, values.foundation, values.dashboard, values.relaySigningKey, values.wssTicketSigningSeed); err == nil {
				t.Fatal("accepted equal credentials")
			}
		})
	}
	if err := validateInventoryToken("inventory", "volunteer", "foundation", "dashboard", "relay-seed", "wss-seed"); err != nil {
		t.Fatalf("distinct credentials rejected: %v", err)
	}
	// The first configured conflict must always win so startup errors are stable.
	err := validateInventoryToken("same", "same", "foundation", "dashboard", "same", "same")
	if err == nil || !strings.Contains(err.Error(), "OPENRUNG_VOLUNTEER_TOKEN") {
		t.Fatalf("conflict error = %v, want volunteer token", err)
	}
}
