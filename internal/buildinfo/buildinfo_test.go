package buildinfo

import (
	"strings"
	"testing"
)

func withInjected(t *testing.T, injectedVersion, injectedRevision string) {
	t.Helper()
	originalVersion, originalRevision := version, revision
	t.Cleanup(func() { version, revision = originalVersion, originalRevision })
	version, revision = injectedVersion, injectedRevision
}

func TestVersionPrefersInjectedValue(t *testing.T) {
	withInjected(t, "1.2.3", "")
	if got := Version("0.1.0"); got != "1.2.3" {
		t.Fatalf("Version = %q, want injected 1.2.3", got)
	}
}

func TestVersionFallsBackToEmbeddedThenDev(t *testing.T) {
	withInjected(t, "", "")
	if got := Version("0.1.0\n"); got != "0.1.0" {
		t.Fatalf("Version = %q, want embedded 0.1.0", got)
	}
	if got := Version("  \n"); got != "dev" {
		t.Fatalf("Version = %q, want dev", got)
	}
}

func TestVersionTrimsInjectedWhitespace(t *testing.T) {
	withInjected(t, " 1.2.3\n", "")
	if got := Version("0.1.0"); got != "1.2.3" {
		t.Fatalf("Version = %q, want trimmed 1.2.3", got)
	}
}

func TestRevisionPrefersInjectedValue(t *testing.T) {
	withInjected(t, "", "abc123")
	if got := Revision(); got != "abc123" {
		t.Fatalf("Revision = %q, want injected abc123", got)
	}
}

func TestRevisionWithoutInjectionIsNeverEmpty(t *testing.T) {
	// Test binaries carry no vcs settings, so this exercises the "unknown"
	// fallback; a source checkout build would report the VCS revision instead.
	withInjected(t, "", "")
	if got := Revision(); got == "" {
		t.Fatal("Revision returned an empty string")
	}
}

func TestInfoFormat(t *testing.T) {
	withInjected(t, "1.2.3", "abc123")
	if got := Info("broker", "0.1.0"); got != "broker/1.2.3 revision=abc123" {
		t.Fatalf("Info = %q", got)
	}
}

func TestInfoDevFallback(t *testing.T) {
	withInjected(t, "", "")
	got := Info("relay", "")
	if !strings.HasPrefix(got, "relay/dev revision=") {
		t.Fatalf("Info = %q, want relay/dev revision=...", got)
	}
}
