package config

import (
	"reflect"
	"testing"
	"time"
)

func TestDiscoveryStaggerMatchesMobile(t *testing.T) {
	// Cross-client tuning constant: must equal the mobile AppConfig's
	// DISCOVERY_STAGGER_MS so every client races discovery identically.
	if DiscoveryStagger != 2500*time.Millisecond {
		t.Fatalf("DiscoveryStagger = %v, want 2.5s (keep in sync with DISCOVERY_STAGGER_MS)", DiscoveryStagger)
	}
}

func TestBrokerCandidates(t *testing.T) {
	https := "https://broker.openrung.org/"

	tests := []struct {
		name    string
		primary string
		want    Candidates
	}{
		{
			name:    "empty primary yields the HTTPS default",
			primary: "",
			want:    Candidates{URLs: []string{https}},
		},
		{
			name:    "blank primary is ignored",
			primary: "   ",
			want:    Candidates{URLs: []string{https}},
		},
		{
			name:    "genuine override is tried first and flagged as an override",
			primary: "https://mirror.example/",
			want:    Candidates{URLs: []string{"https://mirror.example/", https}, OverrideFirst: true},
		},
		{
			name:    "primary echoing a default does not duplicate it or claim the override phase",
			primary: https,
			want:    Candidates{URLs: []string{https}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BrokerCandidates(tc.primary)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BrokerCandidates(%q) = %v, want %v", tc.primary, got, tc.want)
			}
		})
	}
}
