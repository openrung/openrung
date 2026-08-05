package config

import (
	"reflect"
	"testing"
	"time"
)

func TestDiscoveryStaggerSharedDefault(t *testing.T) {
	// Lock the intended shared tuning value. Mobile consumes this same brokerapi
	// constant through its native binding rather than copying it into AppConfig.
	if DiscoveryStagger != 2500*time.Millisecond {
		t.Fatalf("DiscoveryStagger = %v, want shared 2.5s default", DiscoveryStagger)
	}
}

func TestBrokerCandidates(t *testing.T) {
	// The built-in fronts are taken from DefaultBrokerURLs rather than restated,
	// so adding a front does not break these cases. What is asserted here is the
	// ordering policy — defaults in their published order, a genuine override
	// ahead of them, no duplication — not which fronts exist. The order itself
	// is a security property and is asserted in brokerapi.
	defaults := DefaultBrokerURLs
	if len(defaults) < 2 {
		t.Fatalf("expected at least two built-in fronts, got %v", defaults)
	}
	https := defaults[0]
	withOverride := append([]string{"https://mirror.example/"}, defaults...)

	tests := []struct {
		name    string
		primary string
		want    Candidates
	}{
		{
			name:    "empty primary yields the HTTPS defaults",
			primary: "",
			want:    Candidates{URLs: defaults},
		},
		{
			name:    "blank primary is ignored",
			primary: "   ",
			want:    Candidates{URLs: defaults},
		},
		{
			name:    "genuine override is tried first and flagged as an override",
			primary: "https://mirror.example/",
			want:    Candidates{URLs: withOverride, OverrideFirst: true},
		},
		{
			name:    "primary echoing a default does not duplicate it or claim the override phase",
			primary: https,
			want:    Candidates{URLs: defaults},
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
