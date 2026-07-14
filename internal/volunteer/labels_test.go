package volunteer

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

	combinations := len(labelAdjectives) * len(labelNouns)
	if combinations < minLabelCombinations {
		t.Errorf("label namespace = %d combinations (%d adjectives x %d nouns), want at least %d",
			combinations, len(labelAdjectives), len(labelNouns), minLabelCombinations)
	}
}
