package relay

import "testing"

// The read-side EffectiveNodeClass rule lives with the client-facing schema
// and is pinned by brokerapi's own nodeclass test; only the ingest-side
// NormalizeNodeClass below is this package's rule.
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
