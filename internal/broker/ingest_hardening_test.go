package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestRelayIDLedger(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ledger := newRelayIDLedger(time.Hour, 3)

	knows := func(id string, at time.Time) bool {
		_, known := ledger.lookup(id, at)
		return known
	}

	if knows("relay_a", now) {
		t.Fatal("empty ledger unexpectedly knows an ID")
	}
	ledger.remember("relay_a", relay.NodeClassVolunteer, now)
	if class, known := ledger.lookup("relay_a", now); !known || class != relay.NodeClassVolunteer {
		t.Fatalf("lookup = (%q, %v), want retained volunteer class", class, known)
	}
	if !knows("relay_a", now.Add(time.Hour)) {
		t.Fatal("ledger dropped an ID at exactly the TTL")
	}
	if knows("relay_a", now.Add(time.Hour+time.Second)) {
		t.Fatal("ledger kept an ID past the TTL")
	}

	// A heartbeat-style refresh must extend coverage, and a re-registration
	// under a new class (volunteer converted to foundation) must retain the
	// latest attested class.
	ledger.remember("relay_a", relay.NodeClassVolunteer, now)
	ledger.remember("relay_a", relay.NodeClassFoundation, now.Add(time.Hour))
	if class, known := ledger.lookup("relay_a", now.Add(90*time.Minute)); !known || class != relay.NodeClassFoundation {
		t.Fatalf("refreshed lookup = (%q, %v), want retained foundation class", class, known)
	}

	// At capacity with live entries the newcomer is skipped, never an eviction
	// of a still-covered relay.
	ledger.remember("relay_b", relay.NodeClassVolunteer, now.Add(time.Hour))
	ledger.remember("relay_c", relay.NodeClassVolunteer, now.Add(time.Hour))
	ledger.remember("relay_d", relay.NodeClassVolunteer, now.Add(time.Hour))
	if knows("relay_d", now.Add(time.Hour)) {
		t.Fatal("full ledger accepted a newcomer by evicting a live entry")
	}
	if !knows("relay_b", now.Add(time.Hour)) {
		t.Fatal("full ledger lost a live entry")
	}

	// Once entries expire, capacity frees up for newcomers again.
	later := now.Add(3 * time.Hour)
	ledger.remember("relay_e", relay.NodeClassVolunteer, later)
	if !knows("relay_e", later) {
		t.Fatal("ledger did not sweep expired entries to admit a newcomer")
	}
}

func TestGateUnknownRelayTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ledger := newRelayIDLedger(relayLedgerTTL, relayLedgerMaxEntries)
	deadRelay := "relay_" + strings.Repeat("d", 32)
	ledger.remember(deadRelay, relay.NodeClassVolunteer, now.Add(-time.Hour)) // registered, lease since expired

	record := func(relayID, class string) TelemetryRecord {
		return TelemetryRecord{
			ReceivedAt:     now,
			RelayNodeClass: class,
			Event:          TelemetryEvent{EventID: "e-" + relayID, RelayID: relayID},
		}
	}
	records := []TelemetryRecord{
		record("", ""), // no relay reference: kept
		record("relay_"+strings.Repeat("a", 32), "volunteer"), // attested active: kept
		record(deadRelay, ""),                        // recently leased: kept, class stamped
		record("relay_"+strings.Repeat("f", 32), ""), // well-formed but never leased: dropped
		record("Free Tibet VPN — best relay", ""),    // arbitrary string: dropped
	}

	kept, dropped := gateUnknownRelayTelemetry(records, ledger, now)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d records, want 3", len(kept))
	}
	for _, want := range []string{"e-", "e-relay_" + strings.Repeat("a", 32), "e-" + deadRelay} {
		found := false
		for _, r := range kept {
			if r.Event.EventID == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected record %q to survive gating", want)
		}
	}
	// The just-expired relay's record must carry the class the broker attested
	// at its last lease, or the class-filtered dashboard panels would hide the
	// relay exactly when its death is the thing worth investigating.
	for _, r := range kept {
		if r.Event.RelayID == deadRelay && r.RelayNodeClass != relay.NodeClassVolunteer {
			t.Fatalf("ledger-approved record class = %q, want retained %q", r.RelayNodeClass, relay.NodeClassVolunteer)
		}
	}

	// A nil ledger (handler unit tests) disables the gate outright.
	all, dropped := gateUnknownRelayTelemetry(records[3:4], nil, now)
	if dropped != 0 || len(all) != 1 {
		t.Fatalf("nil ledger dropped records: kept=%d dropped=%d", len(all), dropped)
	}
}

// TestTelemetryHandlerDiscardsFabricatedRelayEvents drives the full server:
// events naming the genuinely registered relay pass, events naming IDs the
// broker never minted vanish before they reach the sink, and the accepted
// count reflects what was stored.
func TestTelemetryHandlerDiscardsFabricatedRelayEvents(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: sink})

	registerBody, err := json.Marshal(validRegisterRequest())
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	registerRecorder := httptest.NewRecorder()
	server.ServeHTTP(registerRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(registerBody)))
	if registerRecorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registerRecorder.Code, registerRecorder.Body.String())
	}
	var desc relay.Descriptor
	if err := json.Unmarshal(registerRecorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}

	now := time.Now().UTC()
	event := func(id, relayID string) TelemetryEvent {
		return TelemetryEvent{
			SchemaVersion: 1, EventID: id, Event: "relay_attempt_failed",
			OccurredAt: now, ClientID: "client-1", SessionID: "session-1", RelayID: relayID,
		}
	}
	payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{
		event("real", desc.ID),
		event("fabricated", "relay_"+strings.Repeat("f", 32)),
		event("garbage", "🏴 not a relay id"),
	}})
	if err != nil {
		t.Fatalf("marshal telemetry: %v", err)
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]int
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["accepted"] != 1 {
		t.Fatalf("accepted = %d, want 1", response["accepted"])
	}
	if len(sink.records) != 1 || sink.records[0].Event.EventID != "real" {
		t.Fatalf("sink holds %d records (want just the real relay's): %+v", len(sink.records), sink.records)
	}
	if got := sink.records[0].RelayNodeClass; got != relay.NodeClassVolunteer {
		t.Fatalf("stored record class = %q, want %q", got, relay.NodeClassVolunteer)
	}
}

// TestTelemetryHandlerRejectsUnstorableText pins the poison-batch defense: a
// NUL anywhere in an event would fail the whole asynchronous Postgres insert
// batch on every retry, so it must be rejected at the door.
func TestTelemetryHandlerRejectsUnstorableText(t *testing.T) {
	sink := &memoryTelemetrySink{}
	handler := telemetryHandler(sink, nil, newClientIPResolver(nil), nil)

	base := func() TelemetryEvent {
		return TelemetryEvent{
			SchemaVersion: 1, EventID: "event-1", Event: "connection_attempted",
			OccurredAt: time.Now().UTC(), ClientID: "client-1", SessionID: "session-1",
		}
	}
	nulInAttribute := base()
	nulInAttribute.Attributes = map[string]string{"city": "Teh\x00ran"}
	nulInClientID := base()
	nulInClientID.ClientID = "client\x001"
	nulInAttributeKey := base()
	nulInAttributeKey.Attributes = map[string]string{"ci\x00ty": "Tehran"}

	for name, event := range map[string]TelemetryEvent{
		"attribute value": nulInAttribute,
		"client_id":       nulInClientID,
		"attribute key":   nulInAttributeKey,
	} {
		payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{event}})
		if err != nil {
			t.Fatalf("%s: marshal telemetry: %v", name, err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 for NUL, got %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if len(sink.records) != 0 {
		t.Fatalf("sink stored %d records from rejected batches", len(sink.records))
	}
}

func TestStorableText(t *testing.T) {
	if !storableText("Tehran · ایرانسل") {
		t.Fatal("legitimate non-ASCII text rejected")
	}
	if !storableText("line one\nline two") {
		t.Fatal("multi-line text must stay storable (failure details)")
	}
	if storableText("a\x00b") {
		t.Fatal("NUL accepted")
	}
	if storableText("a\xffb") {
		t.Fatal("invalid UTF-8 accepted")
	}
}

// TestRecordClientSeenRejectsUnstorableHeaders covers the header side door:
// raw HTTP header values may carry non-UTF-8 bytes the JSON decoder would
// have replaced, and they must never reach the sink.
func TestRecordClientSeenRejectsUnstorableHeaders(t *testing.T) {
	sink := &memoryTelemetrySink{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
	req.Header.Set("X-OpenRung-Client-ID", "client\xff1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	recordClientSeen(req, sink, newClientIPResolver(nil), nil)
	if len(sink.records) != 0 {
		t.Fatalf("unstorable client ID was recorded: %+v", sink.records)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	req.Header.Set("X-OpenRung-App-Version", "1.0\xff")
	recordClientSeen(req, sink, newClientIPResolver(nil), nil)
	if len(sink.records) != 1 {
		t.Fatalf("expected one record, got %d", len(sink.records))
	}
	if got := sink.records[0].Event.Attributes["app_version"]; got != "" {
		t.Fatalf("unstorable app_version survived as %q", got)
	}
}
