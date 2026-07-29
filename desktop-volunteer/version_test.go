package main

import (
	"encoding/json"
	"os"
	"testing"
)

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

// VERSION is canonical; wails.json carries an info.productVersion copy only
// because Wails stamps it into the native package metadata. Shipped metadata
// must never disagree with the version the app reports at runtime.
func TestWailsConfigVersionMatchesSource(t *testing.T) {
	version, err := componentVersion()
	if err != nil {
		t.Fatalf("componentVersion: %v", err)
	}

	raw, err := os.ReadFile("wails.json")
	if err != nil {
		t.Fatalf("read wails.json: %v", err)
	}
	var config struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parse wails.json: %v", err)
	}
	if config.Info.ProductVersion != version {
		t.Fatalf("wails.json info.productVersion = %q, want %q from VERSION (VERSION is canonical; update the wails.json copy)", config.Info.ProductVersion, version)
	}
}
