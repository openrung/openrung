package clienttelemetry

import (
	"context"
	"testing"

	"github.com/openrung/openrung/brokerapi"
)

func testPersistentManager(t *testing.T, directory string, broker *outboxTestBroker) (*Manager, *Outbox) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("AppData", tmp)
	outbox, err := NewOutbox(directory, testOutboxFileName, broker.send)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outbox.Close)
	mgr, err := NewWithPlatform(testOutboxBrokerURL, "1.0.0", brokerapi.PlatformNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr.UsePersistentOutbox(outbox)
	return mgr, outbox
}

// A Manager with a persistent outbox routes its whole session through it:
// records land durably, the heartbeat piggybacks under the outbox's identity
// policy, the geo lookup back-patches queued events (the mobile
// TelemetryManager behavior), and Flush drains to the broker.
func TestManagerRoutesSessionsThroughThePersistentOutbox(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	mgr, outbox := testPersistentManager(t, directory, broker)

	if _, err := mgr.BeginSession(); err != nil {
		t.Fatal(err)
	}
	mgr.Record("connection_attempted", "", nil, nil)
	// The geo lookup resolves after the first record: the back-patch must
	// reach the already-queued event.
	mgr.SetGeoAttributes(map[string]string{"country": "JP"})
	mgr.MarkConnected("relay-9")
	mgr.Record("connection_succeeded", "relay-9", map[string]string{"transport": "direct"}, nil)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("records did not land in the outbox: %d pending", got)
	}
	if err := mgr.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("heartbeat did not drain the session's queue: %d pending", got)
	}

	var attempted, succeeded, heartbeats int
	for _, batch := range broker.received() {
		for _, event := range batch {
			switch event.Event {
			case "connection_attempted":
				attempted++
				if event.Attributes["country"] != "JP" {
					t.Fatalf("geo back-patch missed the queued event: %+v", event.Attributes)
				}
			case "connection_succeeded":
				succeeded++
			case "session_heartbeat":
				heartbeats++
			}
		}
	}
	if attempted != 1 || succeeded != 1 || heartbeats != 1 {
		t.Fatalf("events on the wire = attempted:%d succeeded:%d heartbeats:%d", attempted, succeeded, heartbeats)
	}

	mgr.EndSession("disconnect")
	if err := mgr.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("flush left %d events", got)
	}
}

// The durability the mobile outboxes exist for: a session whose flush never
// ran (jetsam kill, expired Shutdown budget) is uploaded by the NEXT session
// sharing the outbox file.
func TestManagerPersistentOutboxCarriesEventsAcrossSessions(t *testing.T) {
	directory := t.TempDir()
	failing := &outboxTestBroker{}
	failing.setFail(context.DeadlineExceeded)
	first, firstBox := testPersistentManager(t, directory, failing)
	if _, err := first.BeginSession(); err != nil {
		t.Fatal(err)
	}
	first.Record("connection_attempted", "", nil, nil)
	first.EndSession("connection_failed")
	if err := first.Flush(context.Background()); err == nil {
		t.Fatal("the failing broker should have failed the flush")
	}
	if got := firstBox.PendingCount(); got != 2 {
		t.Fatalf("unflushed session holds %d events, want 2", got)
	}
	firstBox.Close() // the process dies; the queue belongs to the next open

	working := &outboxTestBroker{}
	second, _ := testPersistentManager(t, directory, working)
	if _, err := second.BeginSession(); err != nil {
		t.Fatal(err)
	}
	second.Record("connection_attempted", "", nil, nil)
	if err := second.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	ids := working.receivedEventIDs()
	if len(ids) != 3 {
		t.Fatalf("the next session should upload its own event plus the stranded 2, got %d", len(ids))
	}
	var ended int
	for _, batch := range working.received() {
		for _, event := range batch {
			if event.Event == "connection_ended" {
				ended++
			}
		}
	}
	if ended != 1 {
		t.Fatal("the stranded session's connection_ended never reached the broker")
	}
}
