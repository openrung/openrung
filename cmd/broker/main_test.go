package main

import "testing"

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
