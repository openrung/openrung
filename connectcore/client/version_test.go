package client

import "testing"

func TestSetAppVersion(t *testing.T) {
	original := appVersion
	defer func() { appVersion = original }()

	SetAppVersion("9.9.9-test")
	if got := AppVersion(); got != "9.9.9-test" {
		t.Fatalf("AppVersion after SetAppVersion = %q, want 9.9.9-test", got)
	}

	// An empty version is ignored so a host passing an unset build variable
	// keeps the current value instead of downgrading to nothing.
	SetAppVersion("")
	if got := AppVersion(); got != "9.9.9-test" {
		t.Fatalf("AppVersion after empty SetAppVersion = %q, want 9.9.9-test", got)
	}
}
