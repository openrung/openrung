package relayruntime

import (
	"strings"
	"testing"

	"openrung/internal/relay"
)

func TestGenerateLabelProducesValidNames(t *testing.T) {
	for i := 0; i < 100; i++ {
		label := GenerateLabel()
		if !strings.Contains(label, "-") {
			t.Fatalf("GenerateLabel() = %q, want adjective-noun form", label)
		}
		normalized, err := relay.NormalizeLabel(label)
		if err != nil {
			t.Fatalf("GenerateLabel() = %q is not a valid label: %v", label, err)
		}
		if normalized != label {
			t.Fatalf("GenerateLabel() = %q changed under NormalizeLabel to %q", label, normalized)
		}
	}
}

// minLabelCombinations guards against the vocabulary shrinking back toward a
// collision-prone size. The adjective-noun namespace should stay comfortably
// large so independently named relays rarely clash.
const minLabelCombinations = 10000

func TestLabelVocabularyIsLargeAndUnique(t *testing.T) {
	assertUnique := func(name string, words []string) {
		seen := make(map[string]bool, len(words))
		for _, w := range words {
			if seen[w] {
				t.Errorf("%s contains duplicate word %q", name, w)
			}
			seen[w] = true
		}
	}
	assertUnique("labelAdjectives", labelAdjectives)
	assertUnique("labelNouns", labelNouns)

	// The vocabulary is now hand-editable data rather than Go literals, and
	// deploy/lib/relay-label.sh reads the same files. Reject any word that
	// would produce a label the relay itself refuses at startup.
	assertUsable := func(name string, words []string) {
		if len(words) == 0 {
			t.Fatalf("%s is empty; the embedded word list failed to load", name)
		}
		for _, w := range words {
			if w != strings.TrimSpace(w) {
				t.Errorf("%s word %q has surrounding whitespace", name, w)
			}
			if w != strings.ToLower(w) {
				t.Errorf("%s word %q is not lowercase", name, w)
			}
			if strings.Contains(w, "-") {
				t.Errorf("%s word %q contains '-', which would break the adjective-noun split", name, w)
			}
			if _, err := relay.NormalizeLabel(w + "-x"); err != nil {
				t.Errorf("%s word %q is not usable in a label: %v", name, w, err)
			}
		}
	}
	assertUsable("labelAdjectives", labelAdjectives)
	assertUsable("labelNouns", labelNouns)

	combinations := len(labelAdjectives) * len(labelNouns)
	if combinations < minLabelCombinations {
		t.Errorf("label namespace = %d combinations (%d adjectives x %d nouns), want at least %d",
			combinations, len(labelAdjectives), len(labelNouns), minLabelCombinations)
	}
}
