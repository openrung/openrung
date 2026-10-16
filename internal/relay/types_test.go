package relay

import (
	"encoding/json"
	"strings"
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

func TestRegistrationLeaseTokenIsPrivateToRegisterResponse(t *testing.T) {
	desc := Descriptor{
		ID:           "relay_stable",
		RelayVersion: "1.2.3",
		LeaseToken:   "lease_secret",
	}

	descriptorJSON, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	var publicFields map[string]any
	if err := json.Unmarshal(descriptorJSON, &publicFields); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if _, ok := publicFields["lease_token"]; ok {
		t.Fatalf("public descriptor leaked lease_token: %s", descriptorJSON)
	}

	registrationJSON, err := json.Marshal(RegisterResponse{Descriptor: desc})
	if err != nil {
		t.Fatalf("marshal registration response: %v", err)
	}
	var privateFields map[string]any
	if err := json.Unmarshal(registrationJSON, &privateFields); err != nil {
		t.Fatalf("decode registration response fields: %v", err)
	}
	if privateFields["lease_token"] != desc.LeaseToken {
		t.Fatalf("registration lease_token = %#v, want %q", privateFields["lease_token"], desc.LeaseToken)
	}

	var decoded RegisterResponse
	if err := json.Unmarshal(registrationJSON, &decoded); err != nil {
		t.Fatalf("unmarshal registration response: %v", err)
	}
	if decoded.Descriptor.LeaseToken != desc.LeaseToken || decoded.Descriptor.RelayVersion != desc.RelayVersion {
		t.Fatalf("registration round trip = %+v, want lease and version preserved", decoded.Descriptor)
	}
}

func TestLegacyRegisterResponseOmitsEmptyLeaseToken(t *testing.T) {
	payload, err := json.Marshal(RegisterResponse{Descriptor: Descriptor{ID: "relay_legacy"}})
	if err != nil {
		t.Fatalf("marshal legacy registration response: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode legacy registration response: %v", err)
	}
	if _, ok := fields["lease_token"]; ok {
		t.Fatalf("legacy response changed wire shape with empty lease_token: %s", payload)
	}
}

// The lease capability is private registration state: it rides the
// registration response beside lease_token, round-trips through
// RegisterResponse, and never appears in a public descriptor.
func TestLeaseCapabilityIsPrivateToRegisterResponse(t *testing.T) {
	desc := Descriptor{ID: "relay_stable", RelayVersion: "1.2.3", LeaseToken: "lease_secret", LeaseCapable: true}
	descriptorJSON, err := json.Marshal(desc)
	if err != nil {
		t.Fatal(err)
	}
	var publicFields map[string]any
	if err := json.Unmarshal(descriptorJSON, &publicFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_leases", "lease_capable"} {
		if _, ok := publicFields[key]; ok {
			t.Fatalf("public descriptor leaked %s: %s", key, descriptorJSON)
		}
	}
	registrationJSON, err := json.Marshal(RegisterResponse{Descriptor: desc})
	if err != nil {
		t.Fatal(err)
	}
	var privateFields map[string]any
	if err := json.Unmarshal(registrationJSON, &privateFields); err != nil {
		t.Fatal(err)
	}
	if privateFields["access_leases"] != true {
		t.Fatalf("registration response access_leases = %#v, want true: %s", privateFields["access_leases"], registrationJSON)
	}
	var decoded RegisterResponse
	if err := json.Unmarshal(registrationJSON, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Descriptor.LeaseCapable || decoded.Descriptor.LeaseToken != "lease_secret" {
		t.Fatalf("decoded registration = %+v", decoded.Descriptor)
	}
	// A broker that predates leases never emits the key, and a relay reads
	// that as "not confirmed".
	legacy, _ := json.Marshal(RegisterResponse{Descriptor: Descriptor{ID: "relay_old", LeaseToken: "lease_x"}})
	if strings.Contains(string(legacy), "access_leases") {
		t.Fatalf("registration without the capability emitted access_leases: %s", legacy)
	}
	var fromLegacy RegisterResponse
	if err := json.Unmarshal(legacy, &fromLegacy); err != nil || fromLegacy.Descriptor.LeaseCapable {
		t.Fatalf("legacy registration decoded lease_capable=%v, %v", fromLegacy.Descriptor.LeaseCapable, err)
	}
	// The request flag is omitted when unset so old brokers see an unchanged
	// registration body.
	request, _ := json.Marshal(RegisterRequest{RelayVersion: "1"})
	if strings.Contains(string(request), "access_leases") {
		t.Fatalf("unset access_leases serialized: %s", request)
	}
	request, _ = json.Marshal(RegisterRequest{RelayVersion: "1", AccessLeases: true})
	if !strings.Contains(string(request), `"access_leases":true`) {
		t.Fatalf("access_leases not serialized: %s", request)
	}
}
