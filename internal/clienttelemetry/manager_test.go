package clienttelemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

type captureTransport struct {
	mu      sync.Mutex
	batches [][]Event
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var b batch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.batches = append(c.batches, b.Events)
	c.mu.Unlock()
	return jsonResponse(r, http.StatusAccepted, `{"accepted":1}`), nil
}

func (c *captureTransport) sizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(c.batches))
	for i, events := range c.batches {
		out[i] = len(events)
	}
	return out
}

func testManager(transport http.RoundTripper, now func() time.Time) *Manager {
	return &Manager{
		poster:     HTTPClient{BaseURL: "https://broker.example.com", HTTP: &http.Client{Transport: transport}},
		appVersion: "test",
		clientID:   "client-test",
		now:        now,
	}
}

func TestRecordRequiresSession(t *testing.T) {
	m := testManager(&captureTransport{}, time.Now)
	m.Record("connection_attempted", "", nil, nil)
	if len(m.outbox) != 0 {
		t.Fatalf("expected no events without a session, got %d", len(m.outbox))
	}
}

func TestOutboxCapsAtMax(t *testing.T) {
	m := testManager(&captureTransport{}, time.Now)
	m.mu.Lock()
	for i := 0; i < maxQueuedEvents+100; i++ {
		m.enqueueLocked(Event{EventID: strconv.Itoa(i)})
	}
	m.mu.Unlock()

	if len(m.outbox) != maxQueuedEvents {
		t.Fatalf("outbox length = %d, want %d", len(m.outbox), maxQueuedEvents)
	}
	// The oldest 100 events should have been dropped, keeping the most recent.
	if m.outbox[0].EventID != "100" {
		t.Fatalf("oldest retained event = %q, want 100", m.outbox[0].EventID)
	}
}

func TestFlushBatchesAt200(t *testing.T) {
	transport := &captureTransport{}
	m := testManager(transport, time.Now)

	m.mu.Lock()
	for i := 0; i < 450; i++ {
		m.outbox = append(m.outbox, Event{EventID: strconv.Itoa(i), ClientID: "c", SessionID: "s"})
	}
	m.mu.Unlock()

	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	sizes := transport.sizes()
	want := []int{uploadBatchSize, uploadBatchSize, 50}
	if len(sizes) != len(want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("batch sizes = %v, want %v", sizes, want)
		}
	}
	if len(m.outbox) != 0 {
		t.Fatalf("outbox not drained: %d remaining", len(m.outbox))
	}
}

func TestHeartbeatRequiresConnected(t *testing.T) {
	transport := &captureTransport{}
	m := testManager(transport, time.Now)
	if _, err := m.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}

	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(transport.sizes()) != 0 {
		t.Fatal("heartbeat must not send before the session is connected")
	}
}

func TestHeartbeatMeasurementsAndDrain(t *testing.T) {
	clock := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	transport := &captureTransport{}
	m := testManager(transport, now)

	if _, err := m.BeginSession(); err != nil { // StartedAt = 12:00:00
		t.Fatalf("begin session: %v", err)
	}
	clock = clock.Add(10 * time.Second)
	m.MarkConnected("relay_123") // ConnectedAt = 12:00:10

	// Queue a couple of events; the heartbeat should send them and drain the outbox.
	m.Record("relay_attempt_failed", "relay_123", nil, nil)
	m.Record("connection_succeeded", "relay_123", nil, nil)

	clock = clock.Add(50 * time.Second) // heartbeat at 12:01:00
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	batches := transport.batches
	if len(batches) == 0 {
		t.Fatal("expected at least one heartbeat batch")
	}
	first := batches[0]
	if len(first) != 3 {
		t.Fatalf("expected 2 queued events + heartbeat, got %d", len(first))
	}
	heartbeat := first[len(first)-1]
	if heartbeat.Event != "session_heartbeat" {
		t.Fatalf("last event = %q, want session_heartbeat", heartbeat.Event)
	}
	if heartbeat.Attributes["connection_state"] != "connected" {
		t.Fatal("heartbeat missing connection_state=connected")
	}
	if heartbeat.Measurements["session_duration_ms"] != 60_000 {
		t.Fatalf("session_duration_ms = %d, want 60000", heartbeat.Measurements["session_duration_ms"])
	}
	if heartbeat.Measurements["connected_duration_ms"] != 50_000 {
		t.Fatalf("connected_duration_ms = %d, want 50000", heartbeat.Measurements["connected_duration_ms"])
	}
	if len(m.outbox) != 0 {
		t.Fatalf("outbox not drained after heartbeat: %d", len(m.outbox))
	}
}

func TestMarkConnectedSwitchesRelayWithoutResettingDuration(t *testing.T) {
	clock := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	m := testManager(&captureTransport{}, func() time.Time { return clock })
	if _, err := m.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	clock = clock.Add(10 * time.Second)
	m.MarkConnected("relay-a")
	clock = clock.Add(20 * time.Second)
	m.MarkConnected("relay-b")

	m.mu.Lock()
	session := *m.session
	m.mu.Unlock()
	if session.RelayID != "relay-b" {
		t.Fatalf("relay id = %q, want relay-b", session.RelayID)
	}
	wantConnectedAt := time.Date(2026, 7, 11, 12, 0, 10, 0, time.UTC)
	if !session.ConnectedAt.Equal(wantConnectedAt) {
		t.Fatalf("connected at = %s, want %s", session.ConnectedAt, wantConnectedAt)
	}
}

func TestTrafficCountersAttachToHeartbeatAndConnectionEnded(t *testing.T) {
	clock := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	transport := &captureTransport{}
	m := testManager(transport, now)

	if _, err := m.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	m.MarkConnected("relay_123")
	var sent, received int64 = 1_500, 42_000
	m.SetTrafficCounters(func() (int64, int64) { return sent, received })

	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	batches := transport.batches
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("expected a single heartbeat, got %+v", transport.sizes())
	}
	heartbeat := batches[0][0]
	if heartbeat.Measurements["bytes_sent"] != 1_500 || heartbeat.Measurements["bytes_received"] != 42_000 {
		t.Fatalf("heartbeat traffic = %d/%d, want 1500/42000", heartbeat.Measurements["bytes_sent"], heartbeat.Measurements["bytes_received"])
	}

	sent, received = 2_500, 99_000
	m.EndSession("disconnect")
	if len(m.outbox) != 1 {
		t.Fatalf("expected connection_ended event, got %d", len(m.outbox))
	}
	ended := m.outbox[0]
	if ended.Measurements["bytes_sent"] != 2_500 || ended.Measurements["bytes_received"] != 99_000 {
		t.Fatalf("connection_ended traffic = %d/%d, want 2500/99000", ended.Measurements["bytes_sent"], ended.Measurements["bytes_received"])
	}
}

func TestZeroTrafficCountersAreOmitted(t *testing.T) {
	meas := map[string]int64{"session_duration_ms": 1}
	addTrafficMeasurements(meas, func() (int64, int64) { return 0, 0 })
	if _, ok := meas["bytes_sent"]; ok {
		t.Fatal("zero bytes_sent should be omitted")
	}
	if _, ok := meas["bytes_received"]; ok {
		t.Fatal("zero bytes_received should be omitted")
	}
	addTrafficMeasurements(meas, nil)
}

func TestEndSessionEmitsConnectionEnded(t *testing.T) {
	clock := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	m := testManager(&captureTransport{}, now)

	if _, err := m.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	clock = clock.Add(5 * time.Second)
	m.MarkConnected("relay_123")
	clock = clock.Add(30 * time.Second)
	m.EndSession("disconnect")

	if len(m.outbox) != 1 {
		t.Fatalf("expected connection_ended event, got %d", len(m.outbox))
	}
	ended := m.outbox[0]
	if ended.Event != "connection_ended" || ended.Attributes["reason"] != "disconnect" {
		t.Fatalf("unexpected event: %+v", ended)
	}
	if ended.Measurements["session_duration_ms"] != 35_000 {
		t.Fatalf("session_duration_ms = %d, want 35000", ended.Measurements["session_duration_ms"])
	}
	if ended.Measurements["connection_duration_ms"] != 30_000 {
		t.Fatalf("connection_duration_ms = %d, want 30000", ended.Measurements["connection_duration_ms"])
	}
	if m.session != nil {
		t.Fatal("session should be cleared after EndSession")
	}
}

func TestGeoAttributesAttachToEvents(t *testing.T) {
	clock := time.Date(2026, 6, 30, 8, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	transport := &captureTransport{}
	m := testManager(transport, now)

	if _, err := m.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	m.SetGeoAttributes(map[string]string{"country": "Japan", "city": "Tokyo", "isp": "au one net"})
	clock = clock.Add(10 * time.Second)
	m.MarkConnected("relay_123")

	// connection_succeeded (via Record) must carry geo.
	m.Record("connection_succeeded", "relay_123", nil, nil)
	if got := m.outbox[len(m.outbox)-1].Attributes["country"]; got != "Japan" {
		t.Fatalf("recorded event country = %q, want Japan", got)
	}

	// Heartbeats must carry geo too (they keep the dashboard row's country/city/isp populated).
	clock = clock.Add(60 * time.Second)
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	last := transport.batches[len(transport.batches)-1]
	heartbeat := last[len(last)-1]
	if heartbeat.Event != "session_heartbeat" {
		t.Fatalf("expected heartbeat, got %q", heartbeat.Event)
	}
	for _, key := range []string{"country", "city", "isp"} {
		if heartbeat.Attributes[key] == "" {
			t.Fatalf("heartbeat missing geo attribute %q: %+v", key, heartbeat.Attributes)
		}
	}
}

func TestNilManagerIsNoOp(t *testing.T) {
	var m *Manager
	session, err := m.BeginSession()
	if err != nil || session != nil {
		t.Fatalf("nil BeginSession = (%v, %v)", session, err)
	}
	m.MarkConnected("relay")
	m.Record("connection_attempted", "", nil, nil)
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("nil Heartbeat: %v", err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("nil Flush: %v", err)
	}
	m.EndSession("disconnect")
	if m.ClientID() != "" {
		t.Fatal("nil ClientID should be empty")
	}
}
