// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

// The control check compares two fetches taken seconds apart. Relay lists carry
// a freshness bound and are re-signed continuously, so comparing raw bytes would
// fail constantly; comparing the relays themselves is what actually answers
// "did both paths reach the same origin".
func TestSameRelayPayloadIgnoresFreshnessButNotContent(t *testing.T) {
	const signingKey = `"key_id":"627405615601c589"`
	relays := func(entries string) []byte {
		return []byte(`{` + signingKey + `,"not_after":"2026-08-03T09:00:00Z","relays":[` + entries + `]}`)
	}
	first := `{"id":"relay-a","public_host":"198.51.100.10"}`
	second := `{"id":"relay-b","public_host":"198.51.100.11"}`

	for _, test := range []struct {
		name  string
		left  []byte
		right []byte
		same  bool
	}{
		{
			name: "identical",
			left: relays(first + "," + second), right: relays(first + "," + second), same: true,
		},
		{
			// Only the envelope moved on. Same origin, later signature.
			name:  "re-signed with a later freshness bound",
			left:  relays(first),
			right: []byte(`{` + signingKey + `,"not_after":"2026-08-03T10:00:00Z","relays":[` + first + `]}`),
			same:  true,
		},
		{
			// Ranking is per-request; order carries no identity.
			name: "same relays in a different order",
			left: relays(first + "," + second), right: relays(second + "," + first), same: true,
		},
		{
			name: "a relay the other path did not return",
			left: relays(first + "," + second), right: relays(first),
		},
		{
			// Same relay id behind a different address is a different relay for
			// this purpose: it would mean the two paths disagree on where to go.
			name:  "same id, different host",
			left:  relays(`{"id":"relay-a","public_host":"198.51.100.10"}`),
			right: relays(`{"id":"relay-a","public_host":"203.0.113.10"}`),
		},
		{
			name:  "a different signing key",
			left:  relays(first),
			right: []byte(`{"key_id":"0000000000000000","not_after":"2026-08-03T09:00:00Z","relays":[` + first + `]}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameRelayPayload(test.left, test.right); got != test.same {
				t.Fatalf("sameRelayPayload = %v, want %v", got, test.same)
			}
		})
	}
}

// Two unparseable bodies must not compare equal, or a front answering with an
// error page on both paths would pass the control check.
func TestSameRelayPayloadRejectsUnparseableBodies(t *testing.T) {
	page := []byte("<!DOCTYPE html><html><title>Page not found</title></html>")
	if sameRelayPayload(page, page) {
		t.Fatal("two unparseable bodies compared equal")
	}
}

func TestOneLineCollapsesAndBounds(t *testing.T) {
	if got := oneLine("broker list relays:\n  <!DOCTYPE html>\n\n  <html>"); got != "broker list relays: <!DOCTYPE html> <html>" {
		t.Fatalf("oneLine = %q", got)
	}
	long := oneLine(strings.Repeat("x", 4000))
	if len(long) > 340 || !strings.HasSuffix(long, "(truncated)") {
		t.Fatalf("oneLine did not bound a long message: %d bytes, %q", len(long), long[max(0, len(long)-20):])
	}
}
