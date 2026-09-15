package clienttelemetry

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// ValidInstallID accepts native UUID text without rewriting its case or value.
func ValidInstallID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
}

// NewWithIdentity uses a host's existing install UUID without resolving desktop
// paths or modifying process environment. The host still must delegate the sole
// session and heartbeat lifecycle to Engine. Use before recording any events.
func NewWithIdentity(brokerURL, appVersion string, platform brokerapi.Platform, platformVersion, installID string, httpClient *http.Client) (*Manager, error) {
	if !ValidInstallID(installID) {
		return nil, errors.New("invalid install UUID")
	}
	m := &Manager{
		hostIdentity: true,
		poster:       HTTPClient{BaseURL: brokerURL, HTTP: httpClient, AppVersion: appVersion, Platform: platform, PlatformVersion: platformVersion},
		appVersion:   appVersion, clientID: installID, now: time.Now,
	}
	m.useReportedTraffic()
	return m, nil
}

// SetHostAttributes installs a metadata supplier before the first session.
// It runs outside manager locks and must promptly return an owned map. The
// engine wraps it to protect release identity fields from host overrides.
func (m *Manager) SetHostAttributes(attributes func() map[string]string) {
	if m != nil {
		m.mu.Lock()
		m.hostAttributes = attributes
		m.mu.Unlock()
	}
}
func (m *Manager) attributes() map[string]string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	supplier := m.hostAttributes
	m.mu.Unlock()
	if supplier != nil {
		return supplier()
	}
	return nil
}

// UpdateTraffic preserves cumulative session high-water marks across a runtime
// counter reset, matching mobile TelemetryManager. Engine retires each run's
// reporter to reject late samples before a successor may start.
func (m *Manager) UpdateTraffic(sent, received int64) {
	if m == nil {
		return
	}
	m.statsMu.Lock()
	m.sent, m.received = max(m.sent, sent), max(m.received, received)
	m.statsMu.Unlock()
}
func (m *Manager) useReportedTraffic() {
	m.SetTrafficCounters(func() (int64, int64) {
		m.statsMu.Lock()
		defer m.statsMu.Unlock()
		return m.sent, m.received
	})
}

// RecordApplicationConnections accepts native-reduced counts. No destinations,
// geo, device attributes or arbitrary event names cross this seam. The outbox
// sanitizes these rows and applies its per-app/per-batch ingestion budget.
func (m *Manager) RecordApplicationConnections(relayID, packageName string, uid int, count int64) {
	if m == nil || packageName == "" || count <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return
	}
	for count > 0 {
		chunk := min(count, int64(100000))
		id, err := newUUID()
		if err != nil {
			return
		}
		event := Event{SchemaVersion: SchemaVersion, EventID: id, Event: "application_connection", OccurredAt: m.now().UTC(), ClientID: m.clientID, SessionID: m.session.ID, RelayID: relayID, Application: packageName, ApplicationID: uid, Measurements: map[string]int64{"connection_count": chunk}}
		m.storeEventLocked(event)
		count -= chunk
	}
}

func (m *Manager) storeEventLocked(event Event) {
	if m.store != nil && m.migrateFallbackLocked() && m.store.Enqueue(event) {
		return
	}
	m.enqueueLocked(event)
}

// SetBrokerURL follows the front that won verified discovery. Engine calls it
// on its session-owned manager, so an old connection cannot retarget a successor.
// In-flight uploads keep their captured endpoint; later uploads use the winner.
func (m *Manager) SetBrokerURL(brokerURL string) error {
	return m.SetBrokerFront(brokerURL, false)
}

// SetBrokerFront updates the endpoint and Azure TLS mode atomically.
func (m *Manager) SetBrokerFront(brokerURL string, azureSNI bool) error {
	if m == nil {
		return nil
	}
	if _, err := brokerapi.EnforceSecureBrokerURL(brokerURL); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poster.BaseURL = brokerURL
	m.poster.AzureSNI = azureSNI
	if m.session != nil {
		m.session.BrokerURL = brokerURL
	}
	return nil
}
