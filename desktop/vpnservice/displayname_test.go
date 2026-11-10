// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"strings"
	"testing"
)

// The cases mirror the mobile app's RelayDescriptor.displayName tests, so the
// desktop service and the mobile native hosts sanitize identically.
func TestRelayDisplayName(t *testing.T) {
	rocket := "\U0001F680"
	for _, tc := range []struct{ name, label, id, want string }{
		{"trims", "  proud-falcon  ", "relay-1", "proud-falcon"},
		{"empty label falls back to id", "", "relay-1", "relay-1"},
		{"blank label falls back to id", "   ", "relay-1", "relay-1"},
		{"id fallback drops the prefix and clamps", "", "relay_03a7324205e2b4d9", "03a7324205e2"},
		{"id fallback is sanitized", "", "relay_‮0123456789abcdef", "0123456789ab"},
		{"label equal to the id counts as absent", "relay_03a7324205e2b4d9", "relay_03a7324205e2b4d9", "03a7324205e2"},
		{"strips controls", "proud\u0007fal\u001Bcon", "relay-1", "proudfalcon"},
		{"strips bidi overrides", "‮npj.yaler", "relay-1", "npj.yaler"},
		{"strips zero-width format characters", "proud​‍falcon", "relay-1", "proudfalcon"},
		{"strips astral tag characters", "proud\U000E0061falcon", "relay-1", "proudfalcon"},
		{"collapses whitespace", "proud \t\n  falcon", "relay-1", "proud falcon"},
		{"collapses non-breaking spaces", "proud  falcon", "relay-1", "proud falcon"},
		{"collapses ideographic spaces", "proud　falcon", "relay-1", "proud falcon"},
		{"clamps to 24 code points", strings.Repeat("x", 80), "relay-1", strings.Repeat("x", 24)},
		{"clamps astral characters whole", strings.Repeat(rocket, 30), "relay-1", strings.Repeat(rocket, 24)},
		{"never splits an astral character", strings.Repeat("x", 23) + rocket + "rocket", "relay-1", strings.Repeat("x", 23) + rocket},
		{"only stripped characters falls back to id", "‮​\u0007", "relay-1", "relay-1"},
	} {
		if got := relayDisplayName(tc.label, tc.id); got != tc.want {
			t.Errorf("%s: relayDisplayName(%q, %q) = %q, want %q", tc.name, tc.label, tc.id, got, tc.want)
		}
	}
}

func TestSanitizeDisplayTextTrimsASpaceLeftByTheClamp(t *testing.T) {
	if got := sanitizeDisplayText("abc def", 4); got != "abc" {
		t.Fatalf("got %q, want the clamp's trailing space trimmed", got)
	}
}
