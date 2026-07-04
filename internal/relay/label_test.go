package relay

import (
	"strings"
	"testing"
)

func TestNormalizeLabelValid(t *testing.T) {
	cases := []struct{ in, want string }{
		{"happy-hippo", "happy-hippo"},
		{"  spaced-out  ", "spaced-out"},
		{"", ""},
		{"Relay_01.tokyo", "Relay_01.tokyo"},
		{strings.Repeat("a", MaxLabelLength), strings.Repeat("a", MaxLabelLength)},
	}
	for _, c := range cases {
		got, err := NormalizeLabel(c.in)
		if err != nil {
			t.Errorf("NormalizeLabel(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeLabelRejectsUnsafe(t *testing.T) {
	cases := []string{
		"<script>alert(1)</script>",
		"has space",
		"emoji😀",
		`quote"`,
		"semi;colon",
		strings.Repeat("a", MaxLabelLength+1),
	}
	for _, in := range cases {
		if _, err := NormalizeLabel(in); err == nil {
			t.Errorf("NormalizeLabel(%q) expected error, got nil", in)
		}
	}
}
