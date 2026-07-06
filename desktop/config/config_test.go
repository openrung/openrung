package config

import (
	"reflect"
	"testing"
)

func TestBrokerCandidates(t *testing.T) {
	https := "https://broker.openrung.org/"
	ip := "http://54.238.185.205:8080/"

	tests := []struct {
		name    string
		primary string
		want    []string
	}{
		{
			name:    "empty primary yields defaults in order",
			primary: "",
			want:    []string{https, ip},
		},
		{
			name:    "blank primary is ignored",
			primary: "   ",
			want:    []string{https, ip},
		},
		{
			name:    "genuine override is tried first",
			primary: "https://mirror.example/",
			want:    []string{"https://mirror.example/", https, ip},
		},
		{
			name:    "primary echoing a default does not reorder defaults",
			primary: ip,
			want:    []string{https, ip},
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
