//go:build linux || darwin

package directsetup

import "testing"

func TestGUIStartupRejectsRoot(t *testing.T) {
	if err := validateGUIEUID(0); err == nil {
		t.Fatal("root GUI startup was accepted")
	}
	if err := validateGUIEUID(501); err != nil {
		t.Fatalf("normal GUI startup was rejected: %v", err)
	}
}
