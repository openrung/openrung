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
