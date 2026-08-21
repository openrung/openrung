// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import "testing"

// TestEffectiveNodeClass pins the read-side rule, which is deliberately not
// the broker's ingest-side NormalizeNodeClass: reading a signed descriptor,
// an unrecognized class must degrade to the volunteer class rather than be
// rejected, and it must degrade in that direction only — the foundation class
// gates the WSS transport, so anything that merely resembles "foundation"
// stays volunteer.
func TestEffectiveNodeClass(t *testing.T) {
	cases := map[string]string{
		NodeClassFoundation: NodeClassFoundation,
		NodeClassVolunteer:  NodeClassVolunteer,
		"":                  NodeClassVolunteer,
		"partner":           NodeClassVolunteer,
		"Foundation":        NodeClassVolunteer,
		"FOUNDATION":        NodeClassVolunteer,
		" foundation":       NodeClassVolunteer,
		"foundation ":       NodeClassVolunteer,
		"foundation\n":      NodeClassVolunteer,
	}
	for in, want := range cases {
		if got := EffectiveNodeClass(in); got != want {
			t.Errorf("EffectiveNodeClass(%q) = %q, want %q", in, got, want)
		}
	}
}
