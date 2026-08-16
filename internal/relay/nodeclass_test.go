package relay

import "testing"

// TestEffectiveNodeClass pins the read-side rule, which is deliberately not
// NormalizeNodeClass: reading a signed descriptor, an unrecognized class must
// degrade to the volunteer class rather than be rejected, and it must degrade
// in that direction only — the foundation class gates the WSS transport, so
// anything that merely resembles "foundation" stays volunteer.
func TestEffectiveNodeClass(t *testing.T) {
	cases := map[string]string{
		NodeClassFoundation: NodeClassFoundation,
		NodeClassVolunteer:  NodeClassVolunteer,
		"":                  NodeClassVolunteer,
		"partner":           NodeClassVolunteer,
		"Foundation":        NodeClassVolunteer,
		"FOUNDATION":        NodeClassVolunteer,
		" foundation":       NodeClassVolunteer,
		"foundation ":       NodeClassVolunteer,
		"foundation\n":      NodeClassVolunteer,
	}
	for in, want := range cases {
		if got := EffectiveNodeClass(in); got != want {
			t.Errorf("EffectiveNodeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeNodeClass(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: NodeClassVolunteer},
		{in: "volunteer", want: NodeClassVolunteer},
		{in: "foundation", want: NodeClassFoundation},
		{in: "  Foundation ", want: NodeClassFoundation},
		{in: "VOLUNTEER", want: NodeClassVolunteer},
		{in: "partner", wantErr: true},
		{in: "foundation ", want: NodeClassFoundation},
		{in: "foundatio", wantErr: true},
	}
	for _, tc := range cases {
		got, err := NormalizeNodeClass(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeNodeClass(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeNodeClass(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeNodeClass(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
