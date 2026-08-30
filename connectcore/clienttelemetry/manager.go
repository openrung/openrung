package clienttelemetry

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
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
	mu            sync.Mutex
	session       *Session
	outbox        []Event
	store         *Outbox
	poster        HTTPClient
	appVersion    string
	clientID      string
	platformLabel string
	geo           map[string]string
	traffic       TrafficCounters
	now           func() time.Time
}

// TrafficCounters reports the session's cumulative tunneled traffic in bytes:
// sent is client-to-relay upload, received is download. Implementations must be
// safe for concurrent use and must not call back into the Manager.
type TrafficCounters func() (sent, received int64)

// New builds a Manager for the given broker. It resolves the persistent client
// id up front so every event in the session shares it.
func New(brokerURL, appVersion string, httpClient *http.Client) (*Manager, error) {
	return NewWithPlatform(brokerURL, appVersion, brokerapi.PlatformNone, httpClient)
}

// NewWithPlatform builds a Manager whose broker requests carry the shared,
// fixed platform marker selected by brokerapi.
func NewWithPlatform(
	brokerURL string,
	appVersion string,
	platform brokerapi.Platform,
	httpClient *http.Client,
) (*Manager, error) {
	clientID, err := ClientID()
	if err != nil {
		return nil, err
	}
	return &Manager{
		poster: HTTPClient{
			BaseURL:    brokerURL,
			HTTP:       httpClient,
			AppVersion: appVersion,
			Platform:   platform,
		},
		appVersion: appVersion,
		clientID:   clientID,
		now:        time.Now,
	}, nil
}

// UsePersistentOutbox routes the manager's queue through the shared on-disk
// outbox (see Outbox): Record enqueues durably, Flush drains identity-
// homogeneous batches, Heartbeat piggybacks through the outbox's policy, and
// SetGeoAttributes back-patches the session's queued events the way the
// mobile TelemetryManagers do. Events recorded by a session that never
// flushed — a jetsam kill, an expired Shutdown budget — are uploaded by the
// next session on the same outbox. Attach before the first Record; the
// in-memory queue remains the default for hosts without a durable directory
// (the CLI's single foreground session).
func (m *Manager) UsePersistentOutbox(store *Outbox) {
	if m == nil || store == nil {
		return
	}
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
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

// SetPlatformLabel attaches attrs["platform"] to every event the manager
// records, so dashboards can tell apart clients that share an operating_system
// label (a terminal session and a GUI session on the same OS). The broker
// stores it as an ordinary free-form attribute; no ingest change is needed.
// Empty (the default) omits the attribute — the desktop app predates the label
// and its events must stay unchanged.
func (m *Manager) SetPlatformLabel(label string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.platformLabel = label
	m.mu.Unlock()
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
	store := m.store
	var sessionID string
	if m.session != nil {
		sessionID = m.session.ID
	}
	m.mu.Unlock()
	if store != nil && sessionID != "" {
		// The mobile geo back-patch: events recorded before the public-IP
		// lookup resolved still get the session's geo. The in-memory queue
		// keeps its merge-at-record behavior — its sessions are short and
		// its events few.
		store.ApplySessionAttributes(sessionID, copied)
	}
}

// SetTrafficCounters registers the source of cumulative session byte counts.
// When set, session_heartbeat and connection_ended events carry bytes_sent /
// bytes_received measurements so the broker dashboard can show per-session
// data usage.
func (m *Manager) SetTrafficCounters(counters TrafficCounters) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.traffic = counters
	m.mu.Unlock()
}

// MarkConnected records the current relay and, on the first promotion only,
// the session's connect time. Mid-session relay failover must not reset the
// connected-duration clock.
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
	if m.session.ConnectedAt.IsZero() {
		m.session.ConnectedAt = m.now()
	}
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
	if m.platformLabel != "" {
		merged["platform"] = m.platformLabel
	}
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
	built := Event{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		Event:         event,
		OccurredAt:    m.now().UTC(),
		ClientID:      m.session.ClientID,
		SessionID:     m.session.ID,
		RelayID:       resolvedRelay,
		Attributes:    merged,
		Measurements:  meas,
	}
	// The store has its own lock and lands events on disk; holding m.mu
	// across it is fine (the store never calls back), and keeps Record atomic
	// with the session snapshot above. Any in-memory fallback events migrate
	// FIRST: newer events must not resume landing in the store while older
	// ones would drain behind them (Flush is store-then-memory), so either
	// the whole stream is in the store, in order, or this event joins the
	// fallback behind its elders.
	if m.store != nil && m.migrateFallbackLocked() && m.store.Enqueue(built) {
		return
	}
	// No store, or the store is unavailable this operation (locked out,
	// unreadable): fall back to the in-memory queue, the behavior a host
	// without a durable directory has always had — a broken outbox must
	// degrade telemetry's durability, never silence it. Flush drains both.
	m.enqueueLocked(built)
}

// migrateFallbackLocked moves the in-memory fallback into the store, oldest
// first, reporting whether the fallback is now empty (an unavailable store
// stops the migration; the remainder keeps its order and later events queue
// behind it). Caller holds m.mu.
func (m *Manager) migrateFallbackLocked() bool {
	for len(m.outbox) > 0 {
		head := m.outbox[0]
		if _, valid := validateOutboxEvent(head); valid && !m.store.Enqueue(head) {
			return false
		}
		// Migrated — or invalid, which no queue can ever send: drop it rather
		// than wedge the migration.
		m.outbox = m.outbox[1:]
	}
	return true
}

// EndSession emits connection_ended with session/connection durations and clears
// the active session. Mirrors TelemetryManager.endSession.
func (m *Manager) EndSession(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	session := m.session
	traffic := m.traffic
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
	addTrafficMeasurements(meas, traffic)
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
	store := m.store
	if store != nil {
		// Migrate the fallback before the heartbeat touches the store, exactly
		// as Record does: a recovered store must not carry (or piggyback) the
		// stream ahead of older events still sitting in memory — migrated,
		// they join the store in order and ride this very piggyback.
		_ = m.migrateFallbackLocked()
		m.mu.Unlock()
		// The outbox owns the piggyback policy: the queue head rides along
		// only when it matches the heartbeat's own identity, so a historical
		// backlog never delays the cadence. The remainder — and whatever an
		// unfinished migration left in memory — drains through Flush afterwards.
		if _, _, err := store.SendHeartbeat(ctx, m.poster.BaseURL, heartbeat); err != nil {
			return err
		}
		return m.Flush(ctx)
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
	m.mu.Lock()
	store := m.store
	m.mu.Unlock()
	if store != nil {
	storeDrain:
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, pending, err := store.FlushNextBatch(ctx, m.poster.BaseURL)
			switch {
			case errors.Is(err, ErrOutboxUnavailable):
				// The store is not ours this operation (locked out, unreadable);
				// its events belong to the owner. The in-memory fallback below
				// is exactly for this — drain it instead of failing.
				break storeDrain
			case err != nil:
				return err
			case pending == 0:
				break storeDrain
			}
		}
		// Fall through: the in-memory queue may hold events recorded while
		// the store was unavailable (or before it was attached).
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
	m.RunHeartbeatLoopGated(ctx, 0, nil)
}

// RunHeartbeatLoopGated is RunHeartbeatLoop with the loop's two host knobs:
// interval overrides the randomized cadence (zero or negative keeps it), and
// gate — when non-nil — runs after each interval elapses, before the
// heartbeat is sent. The connectcore engine passes its pause gate so a
// suspended host uploads nothing and a heartbeat held by the pause is sent on
// resume; a false return ends the loop (the gate observed ctx end). One loop
// serves every caller, so cadence and policy changes cannot fork.
func (m *Manager) RunHeartbeatLoopGated(ctx context.Context, interval time.Duration, gate func(context.Context) bool) {
	if m == nil {
		return
	}
	for {
		delay := interval
		if delay <= 0 {
			delay = nextHeartbeatDelay()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if gate != nil && !gate(ctx) {
			return
		}
		_ = m.Heartbeat(ctx)
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
	if m.platformLabel != "" {
		attrs["platform"] = m.platformLabel
	}
	for k, v := range m.geo {
		attrs[k] = v
	}
	attrs["connection_state"] = "connected"
	meas := map[string]int64{
		"session_duration_ms":   durationMs(m.session.StartedAt, now),
		"connected_duration_ms": durationMs(m.session.ConnectedAt, now),
	}
	addTrafficMeasurements(meas, m.traffic)
	return Event{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		Event:         "session_heartbeat",
		OccurredAt:    now.UTC(),
		ClientID:      m.session.ClientID,
		SessionID:     m.session.ID,
		RelayID:       m.session.RelayID,
		Attributes:    attrs,
		Measurements:  meas,
	}, true
}

// addTrafficMeasurements attaches cumulative session byte counts when a
// traffic source is registered. Zero values are skipped so sessions without
// traffic reporting stay byte-free on the dashboard rather than showing 0.
func addTrafficMeasurements(meas map[string]int64, traffic TrafficCounters) {
	if traffic == nil {
		return
	}
	sent, received := traffic()
	if sent > 0 {
		meas["bytes_sent"] = sent
	}
	if received > 0 {
		meas["bytes_received"] = received
	}
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
