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
	if err := validateInventoryToken("", "volunteer", "foundation", "dashboard"); err != nil {
		t.Fatalf("empty inventory token: %v", err)
	}
	for name, values := range map[string]struct {
		inventory, registration, foundation, dashboard string
	}{
		"volunteer":  {"same", "same", "foundation", "dashboard"},
		"foundation": {"same", "volunteer", "same", "dashboard"},
		"dashboard":  {"same", "volunteer", "foundation", "same"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInventoryToken(values.inventory, values.registration, values.foundation, values.dashboard); err == nil {
				t.Fatal("accepted equal credentials")
			}
		})
	}
	if err := validateInventoryToken("inventory", "volunteer", "foundation", "dashboard"); err != nil {
		t.Fatalf("distinct credentials rejected: %v", err)
	}
}
