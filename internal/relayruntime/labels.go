package relayruntime

import (
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
)

// label_adjectives.txt and label_nouns.txt are the canonical relay-name
// vocabulary, kept as plain one-word-per-line data rather than Go literals so
// the provisioning scripts under deploy/ can read the very same words. Naming
// happens in two places that cannot share a process: the relay names itself at
// startup, and the provisioning helpers must name a cloud VM before any relay
// exists. Sharing this file is what keeps those two paths from drifting apart.
//
// Keep both lists lowercase and within the safe label charset (see
// relay.NormalizeLabel). The lists are intentionally large so the
// adjective-noun namespace is wide and accidental collisions between
// independently named relays stay unlikely.

//go:embed label_adjectives.txt
var labelAdjectivesRaw string

//go:embed label_nouns.txt
var labelNounsRaw string

var (
	labelAdjectives = parseLabelWords(labelAdjectivesRaw)
	labelNouns      = parseLabelWords(labelNounsRaw)
)

// parseLabelWords splits the embedded word list, ignoring blank lines so the
// data files stay forgiving of a trailing newline.
func parseLabelWords(raw string) []string {
	lines := strings.Split(raw, "\n")
	words := make([]string, 0, len(lines))
	for _, line := range lines {
		if word := strings.TrimSpace(line); word != "" {
			words = append(words, word)
		}
	}
	return words
}

// GenerateLabel returns a random "adjective-noun" relay label (e.g.
// "happy-hippo"). Labels are cosmetic and not guaranteed unique.
func GenerateLabel() string {
	return labelPick(labelAdjectives) + "-" + labelPick(labelNouns)
}

func labelPick(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}
