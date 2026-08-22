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
	// The second line names the bundled sing-box; the release workflow's
	// smoke test pins its exact content against the go.mod version and the
	// with_utls tag.
	if got := versionInfo(); !strings.Contains(got, "\nsing-box/") {
		t.Fatalf("versionInfo() = %q, want a bundled sing-box line", got)
	}
}

func TestRunSubcommandRequiresConfig(t *testing.T) {
	// `run` is the bundled sing-box entrypoint the engine's SingBoxRunner
	// re-execs this binary into; its argv contract ("run", "-c", path) is
	// connectcore's, so the subcommand must exist and demand -c.
	err := run([]string{"run"})
	if err == nil || !strings.Contains(err.Error(), "-c") {
		t.Fatalf("run without -c: got %v, want the -c requirement", err)
	}
}
