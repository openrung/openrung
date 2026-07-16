package main

import "testing"

func TestComponentVersionFromSource(t *testing.T) {
	version, err := componentVersion()
	if err != nil {
		t.Fatalf("componentVersion: %v", err)
	}
	if !stableVersionPattern.MatchString(version) {
		t.Fatalf("componentVersion = %q, want X.Y.Z", version)
	}
}

func TestComponentVersionRejectsInvalidSource(t *testing.T) {
	original := sourceVersion
	t.Cleanup(func() { sourceVersion = original })

	sourceVersion = "dev"
	if _, err := componentVersion(); err == nil {
		t.Fatal("componentVersion accepted a non-semantic version")
	}
}
