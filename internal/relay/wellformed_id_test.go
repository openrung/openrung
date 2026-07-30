package relay

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestWellFormedRelayID(t *testing.T) {
	derived := DeriveRelayID(make(ed25519.PublicKey, ed25519.PublicKeySize))
	valid := []string{
		derived,
		"relay_" + strings.Repeat("0", 32),
		"relay_0123456789abcdef0123456789abcdef",
	}
	for _, id := range valid {
		if !WellFormedRelayID(id) {
			t.Errorf("WellFormedRelayID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"relay_",
		"relay_" + strings.Repeat("0", 31),
		"relay_" + strings.Repeat("0", 33),
		"relay_" + strings.Repeat("A", 32), // uppercase hex is never minted
		"relay_" + strings.Repeat("g", 32),
		"RELAY_" + strings.Repeat("0", 32),
		strings.Repeat("0", 38),
		"volunteer_0123456789abcdef0123456789abcdef",
		"relay_0123456789abcdef0123456789abcde\xff",
	}
	for _, id := range invalid {
		if WellFormedRelayID(id) {
			t.Errorf("WellFormedRelayID(%q) = true, want false", id)
		}
	}
}
