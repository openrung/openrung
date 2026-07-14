package relay

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRelayVersionKeepsLegacyJSONField(t *testing.T) {
	values := map[string]any{
		"register request": RegisterRequest{RelayVersion: "1.2.3"},
		"descriptor":       Descriptor{RelayVersion: "1.2.3"},
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Contains(encoded, []byte(`"volunteer_version":"1.2.3"`)) {
				t.Fatalf("legacy volunteer_version field missing from %s", encoded)
			}
			if bytes.Contains(encoded, []byte(`"relay_version"`)) {
				t.Fatalf("unexpected relay_version wire field in %s", encoded)
			}
		})
	}

	var request RegisterRequest
	if err := json.Unmarshal([]byte(`{"volunteer_version":"4.5.6"}`), &request); err != nil {
		t.Fatalf("unmarshal register request: %v", err)
	}
	if request.RelayVersion != "4.5.6" {
		t.Fatalf("RelayVersion = %q, want %q", request.RelayVersion, "4.5.6")
	}
}
