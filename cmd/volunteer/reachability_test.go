package main

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		mode       string
		tunnelFlag bool
		hub        string
		want       string
	}{
		{"", false, "", "direct"},               // no hub, no flags → direct (legacy default)
		{"", false, "hub:9443", "auto"},         // hub set → auto
		{"", true, "hub:9443", "tunnel"},        // -tunnel forces tunnel
		{"direct", false, "hub:9443", "direct"}, // explicit wins
		{"tunnel", false, "", "tunnel"},         // explicit tunnel
		{"AUTO", false, "hub:9443", "auto"},     // case-insensitive
		{"bogus", false, "", "bogus"},           // invalid passes through (Validate rejects)
	}
	for _, c := range cases {
		if got := normalizeMode(c.mode, c.tunnelFlag, c.hub); got != c.want {
			t.Errorf("normalizeMode(%q, %v, %q) = %q, want %q", c.mode, c.tunnelFlag, c.hub, got, c.want)
		}
	}
}
