package relay

import (
	"encoding/json"
	"testing"
)

func TestRegisterRequestRelayVersionJSONMigration(t *testing.T) {
	encoded, err := json.Marshal(RegisterRequest{RelayVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["relay_version"] != "1.2.3" {
		t.Fatalf("relay_version = %#v, want %q", fields["relay_version"], "1.2.3")
	}
	if fields["volunteer_version"] != "1.2.3" {
		t.Fatalf("volunteer_version compatibility alias = %#v, want %q", fields["volunteer_version"], "1.2.3")
	}

	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "canonical", body: `{"relay_version":"2.3.4"}`, want: "2.3.4"},
		{name: "legacy", body: `{"volunteer_version":"3.4.5"}`, want: "3.4.5"},
		{name: "canonical wins", body: `{"relay_version":"4.5.6","volunteer_version":"legacy"}`, want: "4.5.6"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request RegisterRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if request.RelayVersion != test.want {
				t.Fatalf("RelayVersion = %q, want %q", request.RelayVersion, test.want)
			}
		})
	}
}

func TestDescriptorRelayVersionJSONMigration(t *testing.T) {
	encoded, err := json.Marshal(Descriptor{RelayVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["relay_version"] != "1.2.3" {
		t.Fatalf("relay_version = %#v, want %q", fields["relay_version"], "1.2.3")
	}
	if fields["volunteer_version"] != "1.2.3" {
		t.Fatalf("volunteer_version compatibility alias = %#v, want %q", fields["volunteer_version"], "1.2.3")
	}

	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "canonical", body: `{"relay_version":"2.3.4"}`, want: "2.3.4"},
		{name: "legacy", body: `{"volunteer_version":"3.4.5"}`, want: "3.4.5"},
		{name: "canonical wins", body: `{"relay_version":"4.5.6","volunteer_version":"legacy"}`, want: "4.5.6"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var descriptor Descriptor
			if err := json.Unmarshal([]byte(test.body), &descriptor); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if descriptor.RelayVersion != test.want {
				t.Fatalf("RelayVersion = %q, want %q", descriptor.RelayVersion, test.want)
			}
		})
	}
}
