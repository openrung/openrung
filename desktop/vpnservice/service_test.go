package vpnservice

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/desktop/persist"
	"openrung/desktop/proxyconfig"
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

func candidateIDs(cands []relay.Descriptor) []string {
	ids := make([]string, 0, len(cands))
	for _, cand := range cands {
		ids = append(ids, cand.ID)
	}
	return ids
}

func TestFilterCandidatesPinnedID(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := filterCandidates(usable, "JP", "b") // id wins over country
	if err != nil || stage != "" {
		t.Fatalf("pinned id: stage %q err %v", stage, err)
	}
	// Pinned: exactly the target, never a fallback relay.
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("pinned id candidates = %v", candidateIDs(got))
	}
}

func TestFilterCandidatesPinnedIDAbsent(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := filterCandidates(usable, "", "zz")
	if err == nil || stage != "relay_id_filter" {
		t.Fatalf("absent pinned id: stage %q err %v", stage, err)
	}
}

func TestFilterCandidatesCountryKeepsBrokerOrder(t *testing.T) {
	usable := []relay.Descriptor{
		usableRelay("a", "SG", "", "Singapore"),
		usableRelay("b", "JP", "Tokyo", "Japan"),
		usableRelay("c", "sg", "", "Singapore"), // case-insensitive match
		usableRelay("d", "", "", ""),            // geo-less: excluded from a targeted connect
	}
	got, stage, err := filterCandidates(usable, "sg", "")
	if err != nil || stage != "" {
		t.Fatalf("country filter: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("country candidates = %v", ids)
	}
}

func TestFilterCandidatesCountryAbsent(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := filterCandidates(usable, "US", "")
	if err == nil || stage != "relay_geo_filter" {
		t.Fatalf("absent country: stage %q err %v", stage, err)
	}
}

func TestFilterCandidatesAutoKeepsWholeList(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := filterCandidates(usable, "", "")
	if err != nil || stage != "" {
		t.Fatalf("auto: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("auto candidates = %v", ids)
	}
}

func TestUsableRelaysFiltersWithoutReordering(t *testing.T) {
	expired := usableRelay("x", "JP", "", "Japan")
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"), expired, usableRelay("b", "SG", "", "Singapore"))
	got := usableRelays(resp)
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("usable = %v", ids)
	}
}

func TestDemoteRelayMovesFailedToEnd(t *testing.T) {
	cands := []relay.Descriptor{
		usableRelay("a", "JP", "", "Japan"),
		usableRelay("b", "SG", "", "Singapore"),
		usableRelay("c", "DE", "", "Germany"),
	}
	got := demoteRelay(cands, "a")
	if ids := candidateIDs(got); ids[0] != "b" || ids[1] != "c" || ids[2] != "a" {
		t.Fatalf("demoted order = %v", ids)
	}
	// Demoting an id that is not present is a no-op.
	same := demoteRelay(cands, "zz")
	if ids := candidateIDs(same); ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Fatalf("no-op demote order = %v", ids)
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

func TestGetProxyInfoUsesStableConfiguredEndpoint(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "46685")
	s := New()
	s.store = persist.NewInDir(t.TempDir())

	info, err := s.GetProxyInfo()
	if err != nil {
		t.Fatalf("GetProxyInfo: %v", err)
	}
	if info.Host != proxyconfig.Host || info.Port != 46685 || info.Endpoint != "127.0.0.1:46685" {
		t.Fatalf("unexpected proxy info: %+v", info)
	}
	if runtime.GOOS == "windows" {
		if info.ShellIntegration || info.EnableCommand != "" || info.DisableCommand != "" {
			t.Fatalf("unexpected Windows shell integration: %+v", info)
		}
	} else if !info.ShellIntegration || info.EnableCommand == "" || info.DisableCommand != "openrung_proxy_off" {
		t.Fatalf("missing POSIX shell commands: %+v", info)
	}

	// The endpoint is process-stable even if the inherited environment were to
	// change after startup.
	t.Setenv(proxyconfig.PortEnv, "46686")
	again, err := s.GetProxyInfo()
	if err != nil {
		t.Fatalf("GetProxyInfo again: %v", err)
	}
	if again.Port != info.Port {
		t.Fatalf("proxy port changed within one process: %d -> %d", info.Port, again.Port)
	}
}

func TestLocalProxyPortRetriesAfterResolutionFailure(t *testing.T) {
	s := New()
	s.store = persist.NewInDir(t.TempDir())
	t.Setenv(proxyconfig.PortEnv, "not-a-port")
	if _, err := s.localProxyPort(); err == nil {
		t.Fatal("first invalid resolution unexpectedly succeeded")
	}

	t.Setenv(proxyconfig.PortEnv, "46685")
	port, err := s.localProxyPort()
	if err != nil || port != 46685 {
		t.Fatalf("retry = %d, %v; want 46685, nil", port, err)
	}

	// Once resolution succeeds, later calls keep that endpoint even if the
	// inherited environment changes.
	t.Setenv(proxyconfig.PortEnv, "46686")
	pinned, err := s.localProxyPort()
	if err != nil || pinned != port {
		t.Fatalf("successful endpoint was not pinned: %d, %v", pinned, err)
	}
}

func TestGetProxyInfoKeepsEndpointWhenShellHelperCannotBeWritten(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "46685")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.store = persist.NewInDir(filepath.Join(blocker, "openrung"))

	info, err := s.GetProxyInfo()
	if err != nil {
		t.Fatalf("GetProxyInfo: %v", err)
	}
	if info.Endpoint != "127.0.0.1:46685" {
		t.Fatalf("endpoint hidden by helper failure: %+v", info)
	}
	if runtime.GOOS != "windows" && info.ShellIntegrationError == nil {
		t.Fatalf("missing shell helper error: %+v", info)
	}
}

func TestGetProxyInfoSurfacesNonFatalPersistenceWarning(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.store = persist.NewInDir(filepath.Join(blocker, "openrung"))

	info, err := s.GetProxyInfo()
	if err != nil {
		t.Fatalf("GetProxyInfo: %v", err)
	}
	if info.Port <= 0 || info.Endpoint == "" {
		t.Fatalf("persistence failure blocked the endpoint: %+v", info)
	}
	if info.PersistenceWarning == nil || !strings.Contains(*info.PersistenceWarning, "may change next launch") {
		t.Fatalf("missing persistence warning: %+v", info)
	}
}

type fakeProxyController struct {
	supported  bool
	snap       proxymode.Snapshot
	setErr     error
	restoreErr error
	restores   []proxymode.Snapshot
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
	return f.restoreErr
}

func TestCleanupKeepsRecoverySnapshotUntilRestoreSucceeds(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("AppData", tmp)
	store, err := persist.New()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	snap := proxymode.Snapshot{
		Platform: "windows",
		Windows:  &proxymode.WindowsProxyState{ProxyEnable: true, ProxyServer: "10.0.0.1:3128"},
	}
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	proxy := &fakeProxyController{supported: true, restoreErr: errors.New("notify failed")}
	s := New()
	s.proxy = proxy
	s.store = store
	conn := &connection{proxySet: true, snapshotTaken: true, snapshot: snap}

	s.cleanupConn(conn)
	if !conn.proxySet {
		t.Fatal("failed restore must remain pending")
	}
	if _, ok := store.LoadProxySnapshot(); !ok {
		t.Fatal("failed restore must keep the crash-recovery snapshot")
	}

	proxy.restoreErr = nil
	s.cleanupConn(conn)
	if conn.proxySet {
		t.Fatal("successful retry must clear the pending proxy state")
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("successful retry must clear the crash-recovery snapshot")
	}
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

	s.applyProxy(conn, 7890)

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
