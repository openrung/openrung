// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"slices"
	"testing"

	"github.com/openrung/openrung/connectcore/contract"
)

// accessOutcomeVectorsVersion pins the version of
// contract/vectors/access_outcome.json this suite expects; the file's version
// moves with every edit, and this constant with it (contract.LoadVersioned
// enforces the pin, like every other vector suite).
const accessOutcomeVectorsVersion = 2

type accessOutcomeVectors struct {
	Suites     []string `json:"suites"`
	Attributes map[string]struct {
		Events  []string          `json:"events"`
		Tokens  []string          `json:"tokens"`
		Meaning map[string]string `json:"meaning"`
	} `json:"attributes"`
}

// TestAccessOutcomeVectors pins the closed token sets the engine emits to
// the vector file, in order, and requires every token to carry a meaning —
// the attribute is a dashboard grouping key, so a token added in code
// without a vector row (or the reverse) fails here.
func TestAccessOutcomeVectors(t *testing.T) {
	var vectors accessOutcomeVectors
	if err := contract.LoadVersioned(contract.AccessOutcomeVectors, accessOutcomeVectorsVersion, &vectors); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vectors.Suites, []string{"go"}) {
		t.Fatalf("suites = %v, want the Go suite alone (the engine is the only emitter)", vectors.Suites)
	}
	for name, want := range map[string]struct {
		tokens []string
		events []string
	}{
		"access_outcome": {accessOutcomes, []string{"connection_succeeded", "connection_failed"}},
		"relay_source":   {relaySources, []string{"connection_succeeded"}},
	} {
		attr, ok := vectors.Attributes[name]
		if !ok {
			t.Fatalf("vectors lack the %s attribute", name)
		}
		if !slices.Equal(attr.Tokens, want.tokens) {
			t.Errorf("%s tokens = %v, want the engine's %v", name, attr.Tokens, want.tokens)
		}
		if !slices.Equal(attr.Events, want.events) {
			t.Errorf("%s events = %v, want %v", name, attr.Events, want.events)
		}
		for _, token := range attr.Tokens {
			if attr.Meaning[token] == "" {
				t.Errorf("%s token %q has no meaning", name, token)
			}
		}
		for token := range attr.Meaning {
			if !slices.Contains(attr.Tokens, token) {
				t.Errorf("%s meaning %q names no token", name, token)
			}
		}
	}
	if len(vectors.Attributes) != 2 {
		t.Errorf("vectors describe %d attributes, the engine emits 2", len(vectors.Attributes))
	}
}
