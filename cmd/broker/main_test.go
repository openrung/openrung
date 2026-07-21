package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = " 1.2.3 ", " abcdef0 "
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	if got := versionInfo(); got != "broker/1.2.3 revision=abcdef0" {
		t.Fatalf("versionInfo() = %q, want component, version, and revision", got)
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
