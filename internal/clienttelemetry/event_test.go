package clienttelemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// brokerEvent mirrors internal/broker/telemetry.go TelemetryEvent exactly. Since
// the packages cannot import each other, this is the compatibility guard: a
// client Event must marshal into the broker's wire shape.
type brokerEvent struct {
	SchemaVersion   int               `json:"schema_version"`
	EventID         string            `json:"event_id"`
	Event           string            `json:"event"`
	OccurredAt      time.Time         `json:"occurred_at"`
	ClientID        string            `json:"client_id"`
	SessionID       string            `json:"session_id"`
	RelayID         string            `json:"relay_id,omitempty"`
	Application     string            `json:"application_package,omitempty"`
	ApplicationID   int               `json:"application_uid,omitempty"`
	DestinationIP   string            `json:"destination_ip,omitempty"`
	DestinationPort int               `json:"destination_port,omitempty"`
	Protocol        string            `json:"protocol,omitempty"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	Measurements    map[string]int64  `json:"measurements,omitempty"`
}

func TestEventJSONMatchesBrokerContract(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-1",
		Event:         "connection_succeeded",
		OccurredAt:    time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
		ClientID:      "client-1",
		SessionID:     "session-1",
		RelayID:       "relay_123",
		Attributes:    map[string]string{"app_version": "1.2.3"},
		Measurements:  map[string]int64{"broker_fetch_ms": 42},
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var decoded brokerEvent
	dec := json.NewDecoder(strings.NewReader(string(encoded)))
	dec.DisallowUnknownFields() // matches broker decoder; fails on any extra field
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("broker could not decode client event: %v", err)
	}

	if decoded.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", decoded.SchemaVersion)
	}
	if decoded.EventID != "event-1" || decoded.Event != "connection_succeeded" {
		t.Fatalf("unexpected ids: %+v", decoded)
	}
	if decoded.ClientID != "client-1" || decoded.SessionID != "session-1" || decoded.RelayID != "relay_123" {
		t.Fatalf("unexpected identity fields: %+v", decoded)
	}
	if decoded.OccurredAt.IsZero() {
		t.Fatal("occurred_at must not be zero")
	}
	if decoded.Attributes["app_version"] != "1.2.3" || decoded.Measurements["broker_fetch_ms"] != 42 {
		t.Fatalf("unexpected maps: %+v", decoded)
	}
}

func TestDeviceAttributesIncludesCoreFields(t *testing.T) {
	attrs := DeviceAttributes("9.9.9")
	// operating_system is the key the broker dashboard reads for the OS column.
	for _, key := range []string{"app_version", "operating_system", "os", "arch", "timezone"} {
		if attrs[key] == "" {
			t.Fatalf("device attribute %q missing", key)
		}
	}
	if attrs["app_version"] != "9.9.9" {
		t.Fatalf("app_version = %q, want 9.9.9", attrs["app_version"])
	}
	if len(attrs) > 32 {
		t.Fatalf("device attributes exceed broker limit: %d", len(attrs))
	}
}

func TestNewUUIDFormat(t *testing.T) {
	id, err := newUUID()
	if err != nil {
		t.Fatalf("newUUID: %v", err)
	}
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("unexpected uuid format: %q", id)
	}
	other, _ := newUUID()
	if id == other {
		t.Fatal("expected distinct uuids")
	}
}
