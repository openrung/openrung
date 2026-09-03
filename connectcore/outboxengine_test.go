package connectcore

import (
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// The engine-level durability the outbox absorption exists for (ADR-003 A3):
// with TelemetryOutboxDirectory set, a session whose flush never reached the
// broker leaves its events on disk, and a LATER engine on the same directory
// delivers them — the mobile lifecycle where a killed process must not cost
// the session's telemetry.
func TestEngineTelemetryOutboxCarriesStrandedEventsAcrossEngines(t *testing.T) {
	outboxDir := t.TempDir()
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}

	// Engine 1 connects and disconnects against a dead broker: every flush
	// fails, so the whole session — including connection_ended — stays on
	// disk. Shutdown's small budget returns promptly BECAUSE the events are
	// durable (the in-memory queue would have dropped them here).
	first, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	first.TelemetryOutboxDirectory = outboxDir
	if err := first.Connect("http://127.0.0.1:1", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, first, StatusConnected)
	if err := first.Shutdown(200 * time.Millisecond); err == nil {
		t.Fatal("the dead broker should have failed the terminal flush")
	}
	waitIdle(t, first)
	// The next engine takes over the outbox file; release this one's lock the
	// way a process death would.
	if first.outbox == nil {
		t.Fatal("the engine never opened its persistent outbox")
	}
	pending := first.outbox.PendingCount()
	if pending == 0 {
		t.Fatal("the failed session left nothing on disk")
	}
	first.outbox.Close()

	// Engine 2 on the same directory, against a working broker: its own
	// session drains the stranded one.
	sink := newTelemetrySink(t)
	second, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	second.TelemetryOutboxDirectory = outboxDir
	if err := second.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, second, StatusConnected)
	if err := second.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("the working broker should have flushed cleanly: %v", err)
	}
	waitIdle(t, second)

	ended := sink.named("connection_ended")
	if len(ended) != 2 {
		t.Fatalf("connection_ended events = %d; want the stranded session's plus this one's", len(ended))
	}
	sessions := map[string]bool{}
	for _, event := range ended {
		sessions[event.SessionID] = true
	}
	if len(sessions) != 2 {
		t.Fatalf("both sessions' terminal events should reach the broker, got %+v", sessions)
	}
	if second.outbox == nil || second.outbox.PendingCount() != 0 {
		t.Fatal("the shared outbox should be drained")
	}
}
