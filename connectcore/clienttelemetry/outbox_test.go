package clienttelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests moved from the mobile binding
// (openrung-mobile-app/android/punchbridge/telemetry_binding_test.go) with the
// outbox policy they guard (ADR-003 A3); the JSON-string entry points they
// exercised now live in the binding's thin adapter, so they drive the typed
// API directly.

const testOutboxFileName = "openrung_telemetry_outbox.jsonl"

// outboxTestBroker captures every batch the outbox posts through its send
// function, plus the raw wire bytes (for asserting what must NOT be on the
// wire) — and can fail requests or hold them open.
type outboxTestBroker struct {
	mu      sync.Mutex
	batches [][]Event
	bodies  []string
	fail    error
}

func (b *outboxTestBroker) send(ctx context.Context, brokerURL string, events []Event) error {
	body, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{Events: events})
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.batches = append(b.batches, append([]Event(nil), events...))
	b.bodies = append(b.bodies, string(body))
	return nil
}

func (b *outboxTestBroker) setFail(err error) {
	b.mu.Lock()
	b.fail = err
	b.mu.Unlock()
}

func (b *outboxTestBroker) received() [][]Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]Event(nil), b.batches...)
}

func (b *outboxTestBroker) rawBodies() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bodies...)
}

func (b *outboxTestBroker) receivedEventIDs() []string {
	var ids []string
	for _, batch := range b.received() {
		for _, event := range batch {
			ids = append(ids, event.EventID)
		}
	}
	return ids
}

const testOutboxBrokerURL = "https://broker.example.org"

func testOutbox(t *testing.T, directory string, broker *outboxTestBroker) *Outbox {
	t.Helper()
	outbox, err := NewOutbox(directory, testOutboxFileName, broker.send)
	if err != nil {
		t.Fatalf("outbox constructor rejected valid inputs: %v", err)
	}
	t.Cleanup(outbox.Close)
	return outbox
}

func testOutboxEvent(id, event, clientID, sessionID string) Event {
	return Event{
		SchemaVersion: 1,
		EventID:       id,
		Event:         event,
		OccurredAt:    time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		ClientID:      clientID,
		SessionID:     sessionID,
	}
}

func drainOutbox(t *testing.T, outbox *Outbox, brokerURL string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		_, pending, err := outbox.FlushNextBatch(context.Background(), brokerURL)
		if err != nil {
			t.Fatalf("flush failed: %v", err)
		}
		if pending == 0 {
			return
		}
	}
	t.Fatal("outbox did not drain within 20 batches")
}

// The explicit 0.3.5-regression end-to-end test: an outbox file written by
// the pre-binding platform code — including the removed destination_* keys,
// blank lines, and a line torn by a process kill — is uploaded after the
// implementation swap.
func TestOutboxUploadsPreUpgradeNDJSONBacklog(t *testing.T) {
	directory := t.TempDir()
	preUpgrade := strings.Join([]string{
		// The exact shape the Kotlin outbox wrote (encodeDefaults, explicit
		// nulls), including the destination_* fields an even older version
		// persisted before they were removed from the schema.
		`{"schema_version":1,"event_id":"old-1","event":"connection_failed","occurred_at":"2026-08-01T10:00:00.123Z","client_id":"client-a","session_id":"session-1","relay_id":"relay-9","application_package":null,"application_uid":null,"destination_ip":"203.0.113.9","destination_port":443,"protocol":"tcp","attributes":{"failure_reason":"timeout"},"measurements":{"attempt":2}}`,
		``,
		// An application_connection row whose attributes must be scrubbed.
		`{"schema_version":1,"event_id":"old-2","event":"application_connection","occurred_at":"2026-08-01T10:01:00Z","client_id":"client-a","session_id":"session-1","application_package":"com.example.app","application_uid":10002,"attributes":{"stale":"metadata"},"measurements":{"connection_count":3}}`,
		// A line torn mid-write by a process kill: dropped, never fused or fatal.
		`{"schema_version":1,"event_id":"old-3","event":"connection_end`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(directory, testOutboxFileName), []byte(preUpgrade), 0o600); err != nil {
		t.Fatalf("writing pre-upgrade outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("pre-upgrade backlog loaded %d events, want 2", got)
	}
	drainOutbox(t, outbox, testOutboxBrokerURL)

	batches := broker.received()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("expected the backlog in one batch of 2, got %+v", batches)
	}
	first, second := batches[0][0], batches[0][1]
	if first.EventID != "old-1" || second.EventID != "old-2" {
		t.Fatalf("backlog order changed: %q, %q", first.EventID, second.EventID)
	}
	if first.RelayID != "relay-9" || first.Attributes["failure_reason"] != "timeout" ||
		first.Measurements["attempt"] != 2 {
		t.Fatalf("pre-upgrade event fields lost: %+v", first)
	}
	for _, body := range broker.rawBodies() {
		if strings.Contains(body, "203.0.113.9") {
			t.Fatal("removed destination_* payloads must not reach the wire after the upgrade")
		}
	}
	if len(second.Attributes) != 0 {
		t.Fatalf("application_connection attributes must be scrubbed: %+v", second.Attributes)
	}
	if second.Application != "com.example.app" || second.Measurements["connection_count"] != 3 {
		t.Fatalf("application identity lost: %+v", second)
	}
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("outbox still holds %d events after the drain", got)
	}
}

// The older upgrade lineage: the single-JSON-array file iOS wrote before
// 0.3.5 is folded in on first touch, persisted as NDJSON, and uploaded.
func TestOutboxUploadsPreNDJSONArrayBacklog(t *testing.T) {
	directory := t.TempDir()
	legacy := `[` +
		`{"schema_version":1,"event_id":"array-1","event":"connection_ended","occurred_at":"2026-07-01T08:00:00Z","client_id":"client-b","session_id":"session-7","attributes":{"reason":"user_stop"},"measurements":{"session_duration_ms":1200}},` +
		`{"schema_version":1,"event_id":"array-2","event":"session_heartbeat","occurred_at":"2026-07-01T08:01:00Z","client_id":"client-b","session_id":"session-7"}` +
		`]`
	if err := os.WriteFile(filepath.Join(directory, "outbox.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy array outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox, err := NewOutbox(directory, "outbox.json", broker.send)
	if err != nil {
		t.Fatalf("outbox constructor rejected valid inputs: %v", err)
	}
	t.Cleanup(outbox.Close)

	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("legacy array loaded %d events, want 2", got)
	}
	// The load rewrote the array file as NDJSON, so the migration happens once.
	migrated, err := os.ReadFile(filepath.Join(directory, "outbox.json"))
	if err != nil {
		t.Fatalf("reading migrated outbox: %v", err)
	}
	if len(migrated) == 0 || migrated[0] == '[' {
		t.Fatal("legacy array file was not rewritten as NDJSON")
	}
	if got := strings.Count(string(migrated), "\n"); got != 2 {
		t.Fatalf("migrated file holds %d lines, want 2", got)
	}

	drainOutbox(t, outbox, testOutboxBrokerURL)
	if got := broker.receivedEventIDs(); len(got) != 2 || got[0] != "array-1" || got[1] != "array-2" {
		t.Fatalf("legacy events not uploaded in order: %v", got)
	}
}

// The everyday power of the on-disk outbox — what one instance enqueues, the
// next one uploads.
func TestOutboxPersistsAcrossInstances(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	first := testOutbox(t, directory, broker)
	if !first.Enqueue(testOutboxEvent("e-1", "connection_failed", "c", "s")) {
		t.Fatal("enqueue rejected a valid event")
	}
	if !first.Enqueue(testOutboxEvent("e-2", "connection_ended", "c", "s")) {
		t.Fatal("enqueue rejected a valid event")
	}
	first.Close()

	second := testOutbox(t, directory, broker)
	drainOutbox(t, second, testOutboxBrokerURL)
	if got := broker.receivedEventIDs(); len(got) != 2 || got[0] != "e-1" || got[1] != "e-2" {
		t.Fatalf("persisted events not uploaded in order: %v", got)
	}
}

func TestOutboxCapsTheQueueOldestFirst(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	for i := 0; i < OutboxMaxQueued+25; i++ {
		id := fmt.Sprintf("e-%04d", i)
		if !outbox.Enqueue(testOutboxEvent(id, "connection_failed", "c", "s")) {
			t.Fatalf("enqueue %s rejected", id)
		}
	}
	if got := outbox.PendingCount(); got != OutboxMaxQueued {
		t.Fatalf("queue holds %d events, want the %d cap", got, OutboxMaxQueued)
	}
	// A fresh instance sees the same capped queue: the cap survives the file.
	outbox.Close()
	reopened := testOutbox(t, directory, broker)
	if got := reopened.PendingCount(); got != OutboxMaxQueued {
		t.Fatalf("reloaded queue holds %d events, want %d", got, OutboxMaxQueued)
	}
}

func TestOutboxCompactsTheAppendOnlyFile(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	total := outboxCompactThreshold + 10
	for i := 0; i < total; i++ {
		outbox.Enqueue(testOutboxEvent(fmt.Sprintf("e-%04d", i), "x", "c", "s"))
	}
	raw, err := os.ReadFile(filepath.Join(directory, testOutboxFileName))
	if err != nil {
		t.Fatalf("reading outbox file: %v", err)
	}
	lines := strings.Count(string(raw), "\n")
	if lines > outboxCompactThreshold {
		t.Fatalf("file holds %d lines; compaction should bound it at %d", lines, outboxCompactThreshold)
	}
}

func TestOutboxAppliesSessionAttributes(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	mine := testOutboxEvent("mine", "connection_failed", "c", "session-1")
	mine.Attributes = map[string]string{"failure_reason": "timeout"}
	outbox.Enqueue(mine)
	outbox.Enqueue(testOutboxEvent("other", "connection_failed", "c", "session-2"))
	app := testOutboxEvent("app", "application_connection", "c", "session-1")
	app.Application = "com.example.app"
	outbox.Enqueue(app)

	if !outbox.ApplySessionAttributes("session-1", map[string]string{"country": "JP", "isp": "Example"}) {
		t.Fatal("attribute back-patch reported no change")
	}
	outbox.Close()

	reopened := testOutbox(t, directory, broker)
	drainOutbox(t, reopened, testOutboxBrokerURL)
	events := make(map[string]Event)
	for _, batch := range broker.received() {
		for _, event := range batch {
			events[event.EventID] = event
		}
	}
	if events["mine"].Attributes["country"] != "JP" || events["mine"].Attributes["failure_reason"] != "timeout" {
		t.Fatalf("session event not patched: %+v", events["mine"].Attributes)
	}
	if _, patched := events["other"].Attributes["country"]; patched {
		t.Fatal("another session's event must not be patched")
	}
	if len(events["app"].Attributes) != 0 {
		t.Fatalf("application_connection must stay attribute-free: %+v", events["app"].Attributes)
	}
}

func TestOutboxBatchesOneIdentityPrefixAtATime(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	outbox.Enqueue(testOutboxEvent("s1-a", "x", "c", "session-1"))
	outbox.Enqueue(testOutboxEvent("s1-b", "x", "c", "session-1"))
	outbox.Enqueue(testOutboxEvent("s2-a", "x", "c", "session-2"))
	outbox.Enqueue(testOutboxEvent("s2-b", "x", "c", "session-2"))

	drainOutbox(t, outbox, testOutboxBrokerURL)
	batches := broker.received()
	if len(batches) != 2 {
		t.Fatalf("expected 2 identity-homogeneous batches, got %d", len(batches))
	}
	if batches[0][0].SessionID != "session-1" || batches[1][0].SessionID != "session-2" {
		t.Fatalf("batches out of FIFO identity order: %+v", batches)
	}
	for _, batch := range batches {
		for _, event := range batch {
			if event.SessionID != batch[0].SessionID {
				t.Fatalf("mixed identities in one batch: %+v", batch)
			}
		}
	}
}

func TestOutboxDefersApplicationsOverTheFlowBudget(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	appEvent := func(id string, count int64) Event {
		event := testOutboxEvent(id, applicationConnectionEvent, "c", "s")
		event.Application = "com.example.heavy"
		event.Measurements = map[string]int64{applicationCountKey: count}
		return event
	}
	outbox.Enqueue(appEvent("heavy-1", maxReportedFlows))
	outbox.Enqueue(appEvent("heavy-2", 5))
	outbox.Enqueue(testOutboxEvent("plain", "connection_failed", "c", "s"))

	sent, _, err := outbox.FlushNextBatch(context.Background(), testOutboxBrokerURL)
	if err != nil || sent != 2 {
		t.Fatalf("first batch should carry heavy-1 and plain: sent=%d err=%v", sent, err)
	}
	got := broker.receivedEventIDs()
	if len(got) != 2 || got[0] != "heavy-1" || got[1] != "plain" {
		t.Fatalf("budgeted batch selection wrong: %v", got)
	}
	// The deferred application event lands in the next request.
	if _, pending, err := outbox.FlushNextBatch(context.Background(), testOutboxBrokerURL); err != nil || pending != 0 {
		t.Fatalf("deferred event not drained: pending=%d err=%v", pending, err)
	}
}

func TestOutboxHeartbeatPiggybacksOnlyItsOwnIdentity(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	outbox.Enqueue(testOutboxEvent("queued-1", "connection_failed", "c", "session-now"))

	heartbeat := testOutboxEvent("hb-1", "session_heartbeat", "c", "session-now")
	sent, pending, err := outbox.SendHeartbeat(context.Background(), testOutboxBrokerURL, heartbeat)
	if err != nil || sent != 2 || pending != 0 {
		t.Fatalf("heartbeat with same-identity head: sent=%d pending=%d err=%v", sent, pending, err)
	}
	got := broker.receivedEventIDs()
	if len(got) != 2 || got[0] != "queued-1" || got[1] != "hb-1" {
		t.Fatalf("piggyback order wrong: %v", got)
	}

	// A historical head must not delay the cadence: heartbeat goes alone and
	// the backlog stays queued for FlushNextBatch.
	outbox.Enqueue(testOutboxEvent("old-session", "connection_failed", "c", "session-old"))
	heartbeat2 := testOutboxEvent("hb-2", "session_heartbeat", "c", "session-new")
	sent, pending, err = outbox.SendHeartbeat(context.Background(), testOutboxBrokerURL, heartbeat2)
	if err != nil || sent != 1 || pending != 1 {
		t.Fatalf("heartbeat with a historical head: sent=%d pending=%d err=%v", sent, pending, err)
	}
}

func TestOutboxKeepsEventsWhenTheBrokerFails(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	outbox.Enqueue(testOutboxEvent("kept", "connection_failed", "c", "s"))
	broker.setFail(fmt.Errorf("broker returned 500"))

	if _, _, err := outbox.FlushNextBatch(context.Background(), testOutboxBrokerURL); err == nil {
		t.Fatal("a failing broker must fail the flush")
	}
	if got := outbox.PendingCount(); got != 1 {
		t.Fatalf("failed upload must keep the event queued, have %d", got)
	}

	heartbeat := testOutboxEvent("hb", "session_heartbeat", "c", "s")
	if _, _, err := outbox.SendHeartbeat(context.Background(), testOutboxBrokerURL, heartbeat); err == nil {
		t.Fatal("a failing broker must fail the heartbeat")
	}
	if got := outbox.PendingCount(); got != 1 {
		t.Fatalf("failed heartbeat must not commit piggybacked events, have %d", got)
	}

	broker.setFail(nil)
	drainOutbox(t, outbox, testOutboxBrokerURL)
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("recovered flush left %d events queued", got)
	}
}

// The caller deletes the only copy of a pre-file backlog on this answer, so
// "accepted" must mean durably on disk. An unwritable outbox (or a closed
// one) answers -1: keep the legacy store and retry on the next open.
func TestOutboxLegacyImportReportsDurability(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	batch := []Event{testOutboxEvent("legacy-1", "connection_failed", "c", "s")}

	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatalf("making directory unwritable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })

	unwritable := testOutbox(t, directory, broker)
	if got := unwritable.EnqueueBatch(batch); got != -1 {
		t.Fatalf("import into an unwritable outbox reported %d, want -1", got)
	}

	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restoring directory permissions: %v", err)
	}
	writable := testOutbox(t, directory, broker)
	if got := writable.EnqueueBatch(batch); got != 1 {
		t.Fatalf("import into a writable outbox reported %d, want 1", got)
	}
	writable.Close()
	if got := writable.EnqueueBatch(batch); got != -1 {
		t.Fatalf("import into a closed outbox reported %d, want -1", got)
	}
	// A batch with nothing importable answers 0: the import is complete.
	empty := testOutbox(t, directory, broker)
	if got := empty.EnqueueBatch([]Event{{Event: "no-identity"}}); got != 0 {
		t.Fatalf("an unimportable batch reported %d, want 0", got)
	}

	if got := empty.PendingCount(); got != 1 {
		t.Fatalf("durably imported backlog holds %d events, want 1", got)
	}
}

func TestOutboxBoundsBadInput(t *testing.T) {
	broker := &outboxTestBroker{}
	if _, err := NewOutbox("", testOutboxFileName, broker.send); err == nil {
		t.Fatal("empty directory must be rejected")
	}
	if _, err := NewOutbox(t.TempDir(), "", broker.send); err == nil {
		t.Fatal("empty file name must be rejected")
	}
	if _, err := NewOutbox(t.TempDir(), "../escape.jsonl", broker.send); err == nil {
		t.Fatal("a path-traversing file name must be rejected")
	}
	if _, err := NewOutbox(t.TempDir(), testOutboxFileName, nil); err == nil {
		t.Fatal("a nil send function must be rejected")
	}

	outbox := testOutbox(t, t.TempDir(), broker)
	if outbox.Enqueue(Event{Event: "x"}) {
		t.Fatal("an event without identity fields must be dropped")
	}
	if outbox.ApplySessionAttributes("", map[string]string{"a": "b"}) {
		t.Fatal("empty session id must be rejected")
	}
	if outbox.ApplySessionAttributes("s", nil) {
		t.Fatal("empty attributes must be rejected")
	}
	outbox.Enqueue(testOutboxEvent("e", "x", "c", "s"))
	if _, _, err := outbox.FlushNextBatch(context.Background(), "not a url"); err == nil {
		t.Fatal("an invalid broker URL must fail before anything reaches the wire")
	}
	if _, _, err := outbox.SendHeartbeat(context.Background(), testOutboxBrokerURL, Event{Event: "hb"}); err == nil {
		t.Fatal("a heartbeat without identity fields must be rejected")
	}
}

// The batch must not share attribute or measurement maps with the live queue:
// the send marshals outside the outbox lock, and the geo back-patch mutates
// those maps under it — a shared header is a fatal concurrent map access.
func TestOutboxUploadBatchCopiesAttributeMaps(t *testing.T) {
	events := []Event{{
		EventID:      "e-1",
		Event:        "connection_failed",
		ClientID:     "c",
		SessionID:    "s",
		Attributes:   map[string]string{"failure_reason": "timeout"},
		Measurements: map[string]int64{"attempt": 1},
	}}
	batch, _ := outboxUploadBatch(events, 10, outboxBatchByteBudget)
	if len(batch) != 1 {
		t.Fatalf("batch holds %d events, want 1", len(batch))
	}
	events[0].Attributes["country"] = "JP"
	events[0].Measurements["attempt"] = 2
	if _, leaked := batch[0].Attributes["country"]; leaked {
		t.Fatal("batch shares the queue's attribute map")
	}
	if batch[0].Measurements["attempt"] != 1 {
		t.Fatal("batch shares the queue's measurement map")
	}
}

// A transient read failure must not read as an empty queue — the operations
// degrade, the file stays intact, and the next operation after recovery sees
// the backlog.
func TestOutboxLoadFailureDoesNotEraseTheBacklog(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	writer := testOutbox(t, directory, broker)
	writer.Enqueue(testOutboxEvent("b-1", "connection_failed", "c", "session-1"))
	writer.Enqueue(testOutboxEvent("b-2", "connection_ended", "c", "session-1"))
	writer.Close()
	path := filepath.Join(directory, testOutboxFileName)
	intact, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading outbox: %v", err)
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("making outbox unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	outbox := testOutbox(t, directory, broker)
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("unreadable outbox reported %d pending, want 0", got)
	}
	if outbox.Enqueue(testOutboxEvent("b-3", "x", "c", "session-1")) {
		t.Fatal("enqueue must fail while the queue is unreadable")
	}
	if outbox.ApplySessionAttributes("session-1", map[string]string{"country": "JP"}) {
		t.Fatal("the geo back-patch must not run against an unloadable queue")
	}
	if _, _, err := outbox.FlushNextBatch(context.Background(), testOutboxBrokerURL); err == nil {
		t.Fatal("a flush must not report a drained queue it could not read")
	}
	if raw, err := os.ReadFile(path); err == nil && string(raw) != string(intact) {
		t.Fatal("the backlog file changed while unreadable")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restoring outbox permissions: %v", err)
	}
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("recovered outbox holds %d events, want the intact 2", got)
	}
}

// The legacy array is folded in element by element — a row without identity
// fields (which would permanently poison the head batch) and a row Go cannot
// decode are dropped without discarding the decodable remainder.
func TestOutboxArrayMigrationValidatesEachElement(t *testing.T) {
	directory := t.TempDir()
	legacy := `[` +
		`{"schema_version":1,"event_id":"keep-1","event":"connection_failed","occurred_at":"2026-07-01T08:00:00Z","client_id":"c","session_id":"s"},` +
		`{"schema_version":1,"event_id":"no-identity","event":"connection_failed","occurred_at":"2026-07-01T08:00:00Z"},` +
		`{"schema_version":1,"event_id":"bad-time","event":"connection_failed","occurred_at":"yesterday","client_id":"c","session_id":"s"},` +
		`{"schema_version":1,"event_id":"keep-2","event":"connection_ended","occurred_at":"2026-07-01T08:02:00Z","client_id":"c","session_id":"s"}` +
		`]`
	if err := os.WriteFile(filepath.Join(directory, "outbox.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy array outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox, err := NewOutbox(directory, "outbox.json", broker.send)
	if err != nil {
		t.Fatalf("outbox constructor rejected valid inputs: %v", err)
	}
	t.Cleanup(outbox.Close)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("validated migration loaded %d events, want 2", got)
	}
	drainOutbox(t, outbox, testOutboxBrokerURL)
	if got := broker.receivedEventIDs(); len(got) != 2 || got[0] != "keep-1" || got[1] != "keep-2" {
		t.Fatalf("decodable rows lost across the migration: %v", got)
	}
}

// Scrubbing on load must reach the disk even when every line decodes —
// otherwise the removed destination_* keys and application_connection
// attributes survive on the device indefinitely.
func TestOutboxPersistsTheLoadTimeScrub(t *testing.T) {
	directory := t.TempDir()
	preUpgrade := strings.Join([]string{
		`{"schema_version":1,"event_id":"s-1","event":"connection_failed","occurred_at":"2026-08-01T10:00:00Z","client_id":"c","session_id":"s","destination_ip":"203.0.113.9","destination_port":443,"protocol":"tcp"}`,
		`{"schema_version":1,"event_id":"s-2","event":"application_connection","occurred_at":"2026-08-01T10:01:00Z","client_id":"c","session_id":"s","application_package":"com.example.app","attributes":{"stale":"metadata"}}`,
		``,
	}, "\n")
	path := filepath.Join(directory, testOutboxFileName)
	if err := os.WriteFile(path, []byte(preUpgrade), 0o600); err != nil {
		t.Fatalf("writing pre-upgrade outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("backlog loaded %d events, want 2", got)
	}
	scrubbed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading scrubbed outbox: %v", err)
	}
	for _, leaked := range []string{"destination_ip", "destination_port", "protocol", "stale"} {
		if strings.Contains(string(scrubbed), leaked) {
			t.Fatalf("scrubbed data still on disk after load: %q", leaked)
		}
	}
}

// A decodable tail line missing its newline must be re-terminated at load, or
// the next append fuses two events into one undecodable line and loses both.
func TestOutboxRepairsAnUnterminatedTailLine(t *testing.T) {
	directory := t.TempDir()
	tail, err := json.Marshal(testOutboxEvent("t-2", "connection_ended", "c", "s"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := json.Marshal(testOutboxEvent("t-1", "connection_failed", "c", "s"))
	if err != nil {
		t.Fatal(err)
	}
	torn := string(head) + "\n" + string(tail) // no trailing newline
	path := filepath.Join(directory, testOutboxFileName)
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatalf("writing torn outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("torn-tail outbox loaded %d events, want 2", got)
	}
	repaired, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading repaired outbox: %v", err)
	}
	if len(repaired) == 0 || repaired[len(repaired)-1] != '\n' {
		t.Fatal("the unterminated tail line was not re-terminated at load")
	}
	outbox.Enqueue(testOutboxEvent("t-3", "x", "c", "s"))
	outbox.Close()

	reopened := testOutbox(t, directory, broker)
	if got := reopened.PendingCount(); got != 3 {
		t.Fatalf("append after repair fused events: %d survive, want 3", got)
	}
}

// One process owns the outbox file at a time. A second live handle degrades
// to the unavailable path instead of caching its own snapshot and renaming it
// over the owner's queue, and takes over cleanly once the owner closes.
func TestOutboxSecondHandleCannotClobberTheOwner(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	owner := testOutbox(t, directory, broker)
	owner.Enqueue(testOutboxEvent("o-1", "connection_failed", "c", "s"))

	second := testOutbox(t, directory, broker)
	if second.Enqueue(testOutboxEvent("x-1", "x", "c", "s")) {
		t.Fatal("a second handle must not write while the owner holds the lock")
	}
	if got := second.EnqueueBatch([]Event{testOutboxEvent("x-2", "x", "c", "s")}); got != -1 {
		t.Fatalf("a locked-out import reported %d, want -1 (keep the copy)", got)
	}
	if got := second.PendingCount(); got != 0 {
		t.Fatalf("a locked-out handle reported %d pending, want 0", got)
	}
	if _, _, err := second.FlushNextBatch(context.Background(), testOutboxBrokerURL); err == nil {
		t.Fatal("a locked-out flush must fail, not report a drained queue")
	}

	owner.Close()
	if got := second.PendingCount(); got != 1 {
		t.Fatalf("after the owner closed, the second handle sees %d events, want 1", got)
	}
}

// When the load-time migration/repair rewrite fails (disk full, unwritable
// directory), the outbox must not stay loaded — appending NDJSON after a
// legacy array's closing bracket would corrupt the whole backlog. Operations
// degrade, the file stays untouched, and the next operation retries the
// migration.
func TestOutboxStaysUnloadedWhenTheRepairCannotLand(t *testing.T) {
	directory := t.TempDir()
	legacy := `[` +
		`{"schema_version":1,"event_id":"a-1","event":"connection_failed","occurred_at":"2026-07-01T08:00:00Z","client_id":"c","session_id":"s"},` +
		`{"schema_version":1,"event_id":"a-2","event":"connection_ended","occurred_at":"2026-07-01T08:01:00Z","client_id":"c","session_id":"s"}` +
		`]`
	path := filepath.Join(directory, "outbox.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy outbox: %v", err)
	}
	// Pre-create the lock file so ownership can still be taken, then make the
	// directory read-only so the migration's temp-file rewrite cannot land.
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatalf("pre-creating lock file: %v", err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatalf("making directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })

	broker := &outboxTestBroker{}
	outbox, err := NewOutbox(directory, "outbox.json", broker.send)
	if err != nil {
		t.Fatalf("outbox constructor rejected valid inputs: %v", err)
	}
	t.Cleanup(outbox.Close)
	if got := outbox.PendingCount(); got != 0 {
		t.Fatalf("an unrepairable outbox reported %d pending, want 0", got)
	}
	if outbox.Enqueue(testOutboxEvent("a-3", "x", "c", "s")) {
		t.Fatal("enqueue must not append to a file the repair could not land on")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != legacy {
		t.Fatalf("the legacy file changed while unrepairable: %v", err)
	}

	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restoring directory permissions: %v", err)
	}
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("retried load holds %d events, want 2", got)
	}
	if raw, err := os.ReadFile(path); err != nil || len(raw) == 0 || raw[0] == '[' {
		t.Fatal("the retried load did not land the NDJSON migration")
	}
}

// The send runs outside the mutex, so it can succeed while racing Close — and
// Close released the cross-process lock, so another process may own the file
// by then. The commit must refuse to rewrite; re-delivering the accepted
// batch later is the safe side.
func TestOutboxRemoveSentRefusesAfterClose(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	if !outbox.Enqueue(testOutboxEvent("r-1", "connection_failed", "c", "s")) {
		t.Fatal("enqueue rejected a valid event")
	}
	outbox.mu.Lock()
	batch, _ := outboxUploadBatch(outbox.events, outboxBatchSize, outboxBatchByteBudget)
	outbox.mu.Unlock()
	path := filepath.Join(directory, testOutboxFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading outbox: %v", err)
	}

	outbox.Close()
	outbox.removeSent(batch)
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("a closed outbox rewrote the file it no longer owns")
	}
}

// A cancelled context aborts the send and commits nothing — the caller-owned
// cancellation surface the mobile binding wraps in its single-use upload.
func TestOutboxCancelledSendCommitsNothing(t *testing.T) {
	directory := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	sendErr := make(chan error, 1)
	outbox, err := NewOutbox(directory, testOutboxFileName, func(ctx context.Context, _ string, _ []Event) error {
		close(held)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outbox.Close)
	t.Cleanup(func() { close(release) })
	outbox.Enqueue(testOutboxEvent("held", "connection_failed", "c", "s"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _, err := outbox.FlushNextBatch(ctx, testOutboxBrokerURL)
		sendErr <- err
	}()
	<-held
	cancel()
	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("a cancelled send must fail the flush")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock the flush")
	}
	if got := outbox.PendingCount(); got != 1 {
		t.Fatalf("a cancelled send must commit nothing, have %d queued", got)
	}
}

// A truncated legacy array (a process kill mid-write) yields its decodable
// prefix instead of an empty queue durably replacing the backlog — the
// element-by-element salvage extends to the array itself.
func TestOutboxTruncatedLegacyArraySalvagesThePrefix(t *testing.T) {
	directory := t.TempDir()
	truncated := `[` +
		`{"schema_version":1,"event_id":"keep-1","event":"connection_failed","occurred_at":"2026-07-01T08:00:00Z","client_id":"c","session_id":"s"},` +
		`{"schema_version":1,"event_id":"keep-2","event":"connection_ended","occurred_at":"2026-07-01T08:01:00Z","client_id":"c","session_id":"s"},` +
		`{"schema_version":1,"event_id":"torn","event":"conne` // the kill point
	if err := os.WriteFile(filepath.Join(directory, "outbox.json"), []byte(truncated), 0o600); err != nil {
		t.Fatalf("writing truncated legacy outbox: %v", err)
	}

	broker := &outboxTestBroker{}
	outbox, err := NewOutbox(directory, "outbox.json", broker.send)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outbox.Close)
	if got := outbox.PendingCount(); got != 2 {
		t.Fatalf("truncated array salvaged %d events, want the 2 decodable ones", got)
	}
	migrated, err := os.ReadFile(filepath.Join(directory, "outbox.json"))
	if err != nil || len(migrated) == 0 || migrated[0] == '[' {
		t.Fatalf("salvage did not land the NDJSON migration: %v", err)
	}
	drainOutbox(t, outbox, testOutboxBrokerURL)
	if got := broker.receivedEventIDs(); len(got) != 2 || got[0] != "keep-1" || got[1] != "keep-2" {
		t.Fatalf("salvaged prefix lost: %v", got)
	}
}

// A batch the broker permanently refuses (the send function wraps
// ErrBatchRejected) is discarded instead of wedging the queue behind it —
// while a rejected heartbeat piggyback discards nothing, since the rejection
// may be the heartbeat's own.
func TestOutboxDiscardsAPermanentlyRejectedHeadBatch(t *testing.T) {
	directory := t.TempDir()
	broker := &outboxTestBroker{}
	outbox := testOutbox(t, directory, broker)
	outbox.Enqueue(testOutboxEvent("poison", "connection_failed", "c", "session-1"))
	outbox.Enqueue(testOutboxEvent("healthy", "connection_failed", "c", "session-2"))

	broker.setFail(fmt.Errorf("%w: broker says 400", ErrBatchRejected))
	sent, pending, err := outbox.FlushNextBatch(context.Background(), testOutboxBrokerURL)
	if err == nil || !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("rejection must still surface: sent=%d err=%v", sent, err)
	}
	if pending != 1 {
		t.Fatalf("the poison batch must be discarded (pending=%d, want the healthy session's 1)", pending)
	}

	// A rejected heartbeat commits nothing.
	hbSent, hbPending, hbErr := outbox.SendHeartbeat(context.Background(), testOutboxBrokerURL, testOutboxEvent("hb", "session_heartbeat", "c", "session-2"))
	if hbErr == nil || hbSent != 0 || hbPending != 1 {
		t.Fatalf("rejected heartbeat must discard nothing: sent=%d pending=%d err=%v", hbSent, hbPending, hbErr)
	}

	broker.setFail(nil)
	drainOutbox(t, outbox, testOutboxBrokerURL)
	if got := broker.receivedEventIDs(); len(got) != 1 || got[0] != "healthy" {
		t.Fatalf("the queue behind the poison batch never drained: %v", got)
	}
}

// Batch selection bounds the SERIALIZED request size, not just the event
// count: 200 individually valid events can exceed the broker's body cap, and
// a locally-rejected batch would be re-selected forever. Oversized single
// events — unsendable however batched — are discarded rather than retained as
// a permanent wedge.
func TestOutboxBatchSelectionHonorsTheBodyByteBudget(t *testing.T) {
	directory := t.TempDir()
	var maxBody int
	broker := &outboxTestBroker{}
	outbox, err := NewOutbox(directory, testOutboxFileName, func(ctx context.Context, brokerURL string, events []Event) error {
		body, err := json.Marshal(struct {
			Events []Event `json:"events"`
		}{Events: events})
		if err != nil {
			return err
		}
		if len(body) > maxBody {
			maxBody = len(body)
		}
		return broker.send(ctx, brokerURL, events)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outbox.Close)

	// 200 events of ~4KB each: individually fine, together ~800KB — over the
	// 512KB body cap if selected by count alone.
	fat := strings.Repeat("x", 4096)
	for i := 0; i < outboxBatchSize; i++ {
		event := testOutboxEvent(fmt.Sprintf("fat-%03d", i), "connection_failed", "c", "s")
		event.Attributes = map[string]string{"detail": fat}
		outbox.Enqueue(event)
	}
	// One event that can never fit any request: it must be dropped, not
	// retained at the head of every future selection.
	whale := testOutboxEvent("whale", "connection_failed", "c", "s")
	whale.Attributes = map[string]string{"detail": strings.Repeat("y", outboxBatchByteBudget)}
	outbox.Enqueue(whale)

	drainOutbox(t, outbox, testOutboxBrokerURL)
	if maxBody > outboxBatchByteBudget+64 {
		t.Fatalf("a request serialized to %d bytes; the byte budget did not bound it", maxBody)
	}
	batches := broker.received()
	if len(batches) < 2 {
		t.Fatalf("an over-cap queue should split into multiple requests, got %d", len(batches))
	}
	got := broker.receivedEventIDs()
	if len(got) != outboxBatchSize {
		t.Fatalf("delivered %d events, want all %d sendable ones", len(got), outboxBatchSize)
	}
	for _, id := range got {
		if id == "whale" {
			t.Fatal("an unsendable event reached the wire")
		}
	}
	if pendingAfter := outbox.PendingCount(); pendingAfter != 0 {
		t.Fatalf("the unsendable event wedged the queue: %d pending", pendingAfter)
	}
}
