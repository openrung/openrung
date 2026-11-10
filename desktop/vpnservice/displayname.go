// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"strings"
	"unicode"
)

// Display-text limits of the bridge contract, in code points. They match the
// Kotlin, Swift and TypeScript sanitizers of the mobile app, which share the
// UI this service feeds.
const (
	maxRelayNameRunes     = 24
	maxRelayIDNameRunes   = 12
	maxLocationLabelRunes = 128
)

// relayDisplayName renders an operator-supplied relay label safe for the UI,
// falling back to a compact form of the relay id when the label is empty.
// The engine substitutes the full id for an absent label, so a name equal to
// the id counts as absent.
func relayDisplayName(name, id string) string {
	if name != id {
		if clean := sanitizeDisplayText(name, maxRelayNameRunes); clean != "" {
			return clean
		}
	}
	return sanitizeDisplayText(strings.TrimPrefix(id, "relay_"), maxRelayIDNameRunes)
}

// sanitizeDisplayText strips control and format characters (bidi overrides,
// zero-width joiners, tag characters), collapses runs of space separators to
// one space, trims both ends, and clamps the result to maxRunes code points.
func sanitizeDisplayText(raw string, maxRunes int) string {
	collapsed := make([]rune, 0, len(raw))
	pendingSpace := false
	for _, r := range raw {
		switch {
		case unicode.In(r, unicode.Cc, unicode.Cf):
			// Dropped. Tab and newline are controls, so only the space
			// separators below remain as whitespace.
		case unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp):
			pendingSpace = len(collapsed) > 0
		default:
			if pendingSpace {
				collapsed = append(collapsed, ' ')
				pendingSpace = false
			}
			collapsed = append(collapsed, r)
		}
	}
	if len(collapsed) > maxRunes {
		collapsed = collapsed[:maxRunes]
	}
	return strings.TrimRight(string(collapsed), " ")
}
