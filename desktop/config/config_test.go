package config

import (
	"reflect"
	"testing"
)

func TestBrokerCandidates(t *testing.T) {
	https := "https://broker.openrung.org/"

	tests := []struct {
		name    string
		primary string
		want    []string
	}{
		{
			name:    "empty primary yields the HTTPS default",
			primary: "",
			want:    []string{https},
		},
		{
			name:    "blank primary is ignored",
			primary: "   ",
			want:    []string{https},
		},
		{
			name:    "genuine override is tried first",
			primary: "https://mirror.example/",
			want:    []string{"https://mirror.example/", https},
		},
		{
			name:    "primary echoing a default does not duplicate it",
			primary: https,
			want:    []string{https},
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
