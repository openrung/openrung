package main

import (
	"strings"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	// Injection and fallback resolution are internal/buildinfo's tests; this
	// guards the client's wiring: its component name and embedded VERSION.
	want := "client/" + strings.TrimSpace(baseVersion) + " revision="
	if got := versionInfo(); !strings.HasPrefix(got, want) {
		t.Fatalf("versionInfo() = %q, want prefix %q", got, want)
	}
}
