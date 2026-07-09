package vpnservice

import (
	"errors"
	"sync"
	"testing"
	"time"

	"openrung/desktop/proxymode"
	"openrung/internal/relay"
)

func usableRelay(id, countryCode, city, country string) relay.Descriptor {
	return relay.Descriptor{
		ID:               id,
		PublicHost:       "203.0.113.5",
		PublicPort:       443,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         "uuid",
		RealityPublicKey: "pk",
		ShortID:          "sid",
		ServerName:       "sni",
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		ExpiresAt:        time.Now().Add(time.Hour),
		GeoLocation:      relay.GeoLocation{City: city, Country: country, CountryCode: countryCode, Latitude: 1, Longitude: 2},
	}
}

func listOf(relays ...relay.Descriptor) relay.ListResponse {
	return relay.ListResponse{Count: len(relays), ServerTime: time.Now(), Relays: relays}
}

func TestSelectRelayByID(t *testing.T) {
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore"))
	got, err := selectRelay(resp, "", "b")
	if err != nil || got.ID != "b" {
		t.Fatalf("select by id: got %q err %v", got.ID, err)
	}
}

func TestSelectRelayByCountryPrecededByID(t *testing.T) {
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore"))
	// relay id wins over country when both are given.
	got, err := selectRelay(resp, "JP", "b")
	if err != nil || got.ID != "b" {
		t.Fatalf("id should take precedence: got %q err %v", got.ID, err)
	}
}

func TestSelectRelayByCountry(t *testing.T) {
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore"))
	got, err := selectRelay(resp, "sg", "") // case-insensitive
	if err != nil || got.ID != "b" {
		t.Fatalf("select by country: got %q err %v", got.ID, err)
	}
}

func TestSelectRelayAutoFallback(t *testing.T) {
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"))
	got, err := selectRelay(resp, "", "")
	if err != nil || got.ID != "a" {
		t.Fatalf("auto select: got %q err %v", got.ID, err)
	}
}

func TestSelectRelayNoMatch(t *testing.T) {
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"))
	if _, err := selectRelay(resp, "US", ""); err == nil {
		t.Fatal("expected no-match error for absent country")
	}
}

func TestGeoLabelPrefersCityCountry(t *testing.T) {
	if got := geoLabel(usableRelay("a", "JP", "Tokyo", "Japan")); got != "Tokyo, Japan" {
		t.Fatalf("geoLabel = %q", got)
	}
	countryOnly := usableRelay("a", "JP", "", "Japan")
	if got := geoLabel(countryOnly); got != "Japan" {
		t.Fatalf("country-only geoLabel = %q", got)
	}
}

func TestRecentFromRequiresCountryCode(t *testing.T) {
	if recentFrom(usableRelay("a", "", "", "")) != nil {
		t.Fatal("relay without country code should yield no recent")
	}
	r := recentFrom(usableRelay("a", "jp", "Tokyo", "Japan"))
	if r == nil || r.CountryCode != "JP" || r.Label != "Tokyo, Japan" {
		t.Fatalf("unexpected recent: %+v", r)
	}
}

// capturingEmitter collects every emitted state for assertions.
type capturingEmitter struct {
	mu     sync.Mutex
	states []NativeVpnState
}

func (c *capturingEmitter) emit(s NativeVpnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, s)
}

func (c *capturingEmitter) last() NativeVpnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[len(c.states)-1]
}

func TestSetStatusEmitsAndSnapshotsLogs(t *testing.T) {
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit

	s.appendLog("hello")
	s.setStatus(StatusConnecting, keepLabel, clearError)

	last := cap.last()
	if last.Status != StatusConnecting {
		t.Fatalf("status = %q", last.Status)
	}
	if last.LastError != nil {
		t.Fatalf("lastError should be cleared, got %v", *last.LastError)
	}
	// The emitted snapshot includes the ring's log line.
	if len(last.LogLines) != 1 || last.LogLines[0][len(last.LogLines[0])-5:] != "hello" {
		t.Fatalf("expected log line in snapshot, got %v", last.LogLines)
	}
	// Contract: slices are never nil.
	if last.Recents == nil {
		t.Fatal("recents must be a non-nil array")
	}
}

func TestMarkConnectedSetsLabelAndRecent(t *testing.T) {
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit

	recent := recentFrom(usableRelay("a", "JP", "Tokyo", "Japan"))
	s.markConnected("Tokyo, Japan", recent)

	last := cap.last()
	if last.Status != StatusConnected {
		t.Fatalf("status = %q", last.Status)
	}
	if last.RelayLabel == nil || *last.RelayLabel != "Tokyo, Japan" {
		t.Fatalf("relayLabel = %v", last.RelayLabel)
	}
	if len(last.Recents) != 1 || last.Recents[0].CountryCode != "JP" {
		t.Fatalf("recents = %+v", last.Recents)
	}
}

func TestFailedStatusCarriesError(t *testing.T) {
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit
	s.setStatus(StatusFailed, keepLabel, setError("boom"))
	last := cap.last()
	if last.Status != StatusFailed || last.LastError == nil || *last.LastError != "boom" {
		t.Fatalf("failed state not carried: %+v", last)
	}
}

func TestGetIdentityWithoutSession(t *testing.T) {
	restore := clientID
	clientID = func() (string, error) { return "client-xyz", nil }
	defer func() { clientID = restore }()

	s := New()
	id := s.GetIdentity()
	if id.ClientID != "client-xyz" {
		t.Fatalf("clientID = %q", id.ClientID)
	}
	if id.SessionID != nil {
		t.Fatalf("sessionID should be nil when idle, got %v", *id.SessionID)
	}
}

type fakeProxyController struct {
	supported bool
	snap      proxymode.Snapshot
	setErr    error
	restores  []proxymode.Snapshot
}

func (f *fakeProxyController) Supported() bool { return f.supported }

func (f *fakeProxyController) Snapshot() (proxymode.Snapshot, error) {
	return f.snap, nil
}

func (f *fakeProxyController) Set(host string, port int) error {
	return f.setErr
}

func (f *fakeProxyController) Restore(snap proxymode.Snapshot) error {
	f.restores = append(f.restores, snap)
	return nil
}

func TestApplySystemProxyRestoresSnapshotWhenSetFails(t *testing.T) {
	snap := proxymode.Snapshot{
		Platform: "windows",
		Windows: &proxymode.WindowsProxyState{
			ProxyEnable: true,
			ProxyServer: "10.0.0.1:3128",
		},
	}
	proxy := &fakeProxyController{
		supported: true,
		snap:      snap,
		setErr:    errors.New("notify failed after write"),
	}
	s := New()
	s.proxy = proxy
	conn := &connection{}

	s.applySystemProxy(conn, 7890)

	if conn.proxySet {
		t.Fatal("connection should not be marked proxySet when Set fails")
	}
	if len(proxy.restores) != 1 {
		t.Fatalf("expected failed Set to restore snapshot once, got %d restores", len(proxy.restores))
	}
	if got := proxy.restores[0]; got.Platform != snap.Platform || got.Windows == nil || *got.Windows != *snap.Windows {
		t.Fatalf("restored snapshot = %+v, want %+v", got, snap)
	}
}
