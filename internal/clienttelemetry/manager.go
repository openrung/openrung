package clienttelemetry

import (
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

const (
	maxQueuedEvents = 500
	uploadBatchSize = 200
	heartbeatMin    = 50 * time.Second
	heartbeatMax    = 70 * time.Second
)

// Session holds the identity and timing for one connect run, mirroring the
// Android TelemetryManager.Session.
type Session struct {
	ID          string
	ClientID    string
	BrokerURL   string
	RelayID     string
	StartedAt   time.Time
	ConnectedAt time.Time
}

// Manager owns the session lifecycle and the in-memory outbox. It is the CLI
// analog of the Android TelemetryManager. All methods are safe for concurrent
// use. The outbox lives only for the process; the CLI connect command is a
// single foreground session, so events are flushed on success, on each
// heartbeat, and on shutdown rather than persisted to disk.
type Manager struct {
	mu         sync.Mutex
	session    *Session
	outbox     []Event
	poster     HTTPClient
	appVersion string
	clientID   string
	geo        map[string]string
	now        func() time.Time
}

// New builds a Manager for the given broker. It resolves the persistent client
// id up front so every event in the session shares it.
func New(brokerURL, appVersion string, httpClient *http.Client) (*Manager, error) {
	clientID, err := ClientID()
	if err != nil {
		return nil, err
	}
	return &Manager{
		poster:     HTTPClient{BaseURL: brokerURL, HTTP: httpClient},
		appVersion: appVersion,
		clientID:   clientID,
		now:        time.Now,
	}, nil
}

// ClientID returns the resolved persistent client identifier.
func (m *Manager) ClientID() string {
	if m == nil {
		return ""
	}
	return m.clientID
}

// BeginSession starts a new session and returns it. Identity headers for the
// relay-list request come from the returned session. A nil Manager (telemetry
// unavailable) returns a nil session so callers can stay branch-free.
func (m *Manager) BeginSession() (*Session, error) {
	if m == nil {
		return nil, nil
	}
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := &Session{
		ID:        id,
		ClientID:  m.clientID,
		BrokerURL: m.poster.BaseURL,
		StartedAt: m.now(),
	}
	m.session = session
	copied := *session
	return &copied, nil
}

// SetGeoAttributes attaches public-IP geo attributes (country, city, isp, ...)
// to every event the session reports, mirroring the Android TelemetryManager
// geoAttributes. Best-effort: a nil/empty map is ignored.
func (m *Manager) SetGeoAttributes(geo map[string]string) {
	if m == nil || len(geo) == 0 {
		return
	}
	copied := make(map[string]string, len(geo))
	for k, v := range geo {
		copied[k] = v
	}
	m.mu.Lock()
	m.geo = copied
	m.mu.Unlock()
}

// MarkConnected records the relay the session connected to and the connect time.
func (m *Manager) MarkConnected(relayID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return
	}
	m.session.RelayID = relayID
	m.session.ConnectedAt = m.now()
}

// Record enqueues a telemetry event for the active session. Device attributes
// are merged in first so caller-supplied attributes win on conflict.
func (m *Manager) Record(event, relayID string, attrs map[string]string, meas map[string]int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return
	}

	merged := DeviceAttributes(m.appVersion)
	for k, v := range m.geo {
		merged[k] = v
	}
	for k, v := range attrs {
		merged[k] = v
	}

	resolvedRelay := relayID
	if resolvedRelay == "" {
		resolvedRelay = m.session.RelayID
	}

	eventID, err := newUUID()
	if err != nil {
		return
	}
	m.enqueueLocked(Event{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		Event:         event,
		OccurredAt:    m.now().UTC(),
		ClientID:      m.session.ClientID,
		SessionID:     m.session.ID,
		RelayID:       resolvedRelay,
		Attributes:    merged,
		Measurements:  meas,
	})
}

// EndSession emits connection_ended with session/connection durations and clears
// the active session. Mirrors TelemetryManager.endSession.
func (m *Manager) EndSession(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	session := m.session
	now := m.now()
	m.mu.Unlock()
	if session == nil {
		return
	}

	meas := map[string]int64{
		"session_duration_ms": durationMs(session.StartedAt, now),
	}
	if !session.ConnectedAt.IsZero() {
		meas["connection_duration_ms"] = durationMs(session.ConnectedAt, now)
	}
	m.Record("connection_ended", session.RelayID, map[string]string{"reason": reason}, meas)

	m.mu.Lock()
	if m.session != nil && m.session.ID == session.ID {
		m.session = nil
	}
	m.mu.Unlock()
}

// Heartbeat sends a session_heartbeat plus up to uploadBatchSize-1 queued events,
// then drains any remaining outbox. It is a no-op until the session is connected.
// Mirrors TelemetryManager.sendHeartbeat + buildSessionHeartbeat.
func (m *Manager) Heartbeat(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	heartbeat, ok := m.buildHeartbeatLocked()
	if !ok {
		m.mu.Unlock()
		return nil
	}
	queued := m.snapshotLocked(uploadBatchSize - 1)
	m.mu.Unlock()

	if err := m.poster.Send(ctx, append(queued, heartbeat)); err != nil {
		return err
	}
	if len(queued) > 0 {
		m.removeSent(queued)
		return m.Flush(ctx)
	}
	return nil
}

// Flush drains the outbox to the broker in batches of at most uploadBatchSize.
func (m *Manager) Flush(ctx context.Context) error {
	if m == nil {
		return nil
	}
	for {
		m.mu.Lock()
		batch := m.snapshotLocked(uploadBatchSize)
		m.mu.Unlock()
		if len(batch) == 0 {
			return nil
		}
		if err := m.poster.Send(ctx, batch); err != nil {
			return err
		}
		m.removeSent(batch)
	}
}

// RunHeartbeatLoop sends heartbeats on the randomized Android cadence until ctx
// is cancelled. Mirrors OpenRungVpnService.startHeartbeatLoop.
func (m *Manager) RunHeartbeatLoop(ctx context.Context) {
	if m == nil {
		return
	}
	for {
		timer := time.NewTimer(nextHeartbeatDelay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			_ = m.Heartbeat(ctx)
		}
	}
}

func (m *Manager) buildHeartbeatLocked() (Event, bool) {
	if m.session == nil || m.session.RelayID == "" || m.session.ConnectedAt.IsZero() {
		return Event{}, false
	}
	eventID, err := newUUID()
	if err != nil {
		return Event{}, false
	}
	now := m.now()
	attrs := DeviceAttributes(m.appVersion)
	for k, v := range m.geo {
		attrs[k] = v
	}
	attrs["connection_state"] = "connected"
	return Event{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		Event:         "session_heartbeat",
		OccurredAt:    now.UTC(),
		ClientID:      m.session.ClientID,
		SessionID:     m.session.ID,
		RelayID:       m.session.RelayID,
		Attributes:    attrs,
		Measurements: map[string]int64{
			"session_duration_ms":   durationMs(m.session.StartedAt, now),
			"connected_duration_ms": durationMs(m.session.ConnectedAt, now),
		},
	}, true
}

func (m *Manager) enqueueLocked(event Event) {
	m.outbox = append(m.outbox, event)
	if len(m.outbox) > maxQueuedEvents {
		m.outbox = append([]Event(nil), m.outbox[len(m.outbox)-maxQueuedEvents:]...)
	}
}

// snapshotLocked returns a copy of up to limit leading outbox events.
func (m *Manager) snapshotLocked(limit int) []Event {
	n := len(m.outbox)
	if n > limit {
		n = limit
	}
	if n == 0 {
		return nil
	}
	return append([]Event(nil), m.outbox[:n]...)
}

func (m *Manager) removeSent(sent []Event) {
	sentIDs := make(map[string]struct{}, len(sent))
	for _, event := range sent {
		sentIDs[event.EventID] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.outbox[:0]
	for _, event := range m.outbox {
		if _, ok := sentIDs[event.EventID]; !ok {
			kept = append(kept, event)
		}
	}
	m.outbox = kept
}

func durationMs(from, to time.Time) int64 {
	ms := to.Sub(from).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

func nextHeartbeatDelay() time.Duration {
	return heartbeatMin + time.Duration(rand.Int63n(int64(heartbeatMax-heartbeatMin)+1))
}
