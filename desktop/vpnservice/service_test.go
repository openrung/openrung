package vpnservice

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/proxyconfig"
	"openrung/internal/clientstate"
	"openrung/internal/proxymode"
)

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

func TestStateChangeEmitsSnapshotWithLogs(t *testing.T) {
	// The sink merges an engine state change with the log ring, so every
	// emitted snapshot carries the latest console lines.
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit

	s.appendLog("hello")
	sink := &engineSink{s: s}
	sink.StateChanged(connectcore.State{Status: connectcore.StatusConnecting, Recents: []connectcore.RecentNode{}})

	last := cap.last()
	if last.Status != StatusConnecting {
		t.Fatalf("status = %q", last.Status)
	}
	if last.LastError != nil {
		t.Fatalf("lastError should be nil, got %v", *last.LastError)
	}
	// The emitted snapshot includes the ring's log line, timestamp-stamped.
	if len(last.LogLines) != 1 || !strings.HasSuffix(last.LogLines[0], "hello") {
		t.Fatalf("expected log line in snapshot, got %v", last.LogLines)
	}
	// Contract: slices are never nil.
	if last.Recents == nil {
		t.Fatal("recents must be a non-nil array")
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

func TestGetDiagnosticsReturnsTheEngineReport(t *testing.T) {
	s := New()
	text := s.GetDiagnostics()
	if !strings.HasPrefix(text, "OpenRung diagnostics\n") || !strings.Contains(text, "status: disconnected") {
		t.Fatalf("diagnostics = %q", text)
	}
}

func TestGetProxyInfoUsesStableConfiguredEndpoint(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "46685")
	s := New()
	s.store = clientstate.NewInDir(t.TempDir())

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

func TestGetProxyInfoKeepsEndpointWhenShellHelperCannotBeWritten(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "46685")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.store = clientstate.NewInDir(filepath.Join(blocker, "openrung"))

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

// withStore wires a store the way Startup does: one value backs both the shell
// helper and the engine's persistence hook.
func withStore(s *Service, store *clientstate.Store) *Service {
	s.store = store
	s.engine.Persistence = storeAdapter{store: store}
	return s
}

func TestGetProxyInfoSurfacesNonFatalPersistenceWarning(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The configuration directory cannot be created.
	s := withStore(New(), clientstate.NewInDir(filepath.Join(blocker, "openrung")))

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

// The desktop app must offer the same address on the next launch, since users
// configure it in a browser.
func TestGetProxyInfoReusesThePersistedEndpointOnTheNextLaunch(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")
	dir := t.TempDir()

	first := withStore(New(), clientstate.NewInDir(dir))
	info, err := first.GetProxyInfo()
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if info.Port <= 0 {
		t.Fatalf("no endpoint allocated: %+v", info)
	}
	if info.PersistenceWarning != nil {
		t.Fatalf("writable store still warned: %s", *info.PersistenceWarning)
	}

	// A second Service over the same directory stands in for the next launch.
	next := withStore(New(), clientstate.NewInDir(dir))
	again, err := next.GetProxyInfo()
	if err != nil {
		t.Fatalf("next launch: %v", err)
	}
	if again.Port != info.Port {
		t.Fatalf("endpoint changed across launches: %d -> %d", info.Port, again.Port)
	}
}

func TestStoreAdapterRoundTripsSnapshotAndRecents(t *testing.T) {
	// The engine moves the proxy snapshot around as an opaque value; the
	// adapter must hand internal/clientstate the same proxymode shape (and
	// on-disk format) it stored before the engine extraction.
	adapter := storeAdapter{store: clientstate.NewInDir(t.TempDir())}
	snap := proxymode.Snapshot{
		Platform: "windows",
		Windows:  &proxymode.WindowsProxyState{ProxyEnable: true, ProxyServer: "10.0.0.1:3128"},
	}
	if err := adapter.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	loaded, ok := adapter.LoadProxySnapshot()
	if !ok {
		t.Fatal("snapshot not persisted")
	}
	typed, ok := loaded.(proxymode.Snapshot)
	if !ok || typed.Platform != snap.Platform || typed.Windows == nil || *typed.Windows != *snap.Windows {
		t.Fatalf("loaded snapshot = %+v, want %+v", loaded, snap)
	}
	if err := adapter.SaveProxySnapshot(struct{}{}); err == nil {
		t.Fatal("a non-proxymode snapshot must be refused, never mis-persisted")
	}
	if err := adapter.ClearProxySnapshot(); err != nil {
		t.Fatalf("clear snapshot: %v", err)
	}
	if _, ok := adapter.LoadProxySnapshot(); ok {
		t.Fatal("snapshot survived clear")
	}

	recents := []connectcore.RecentNode{
		{RelayID: "relay_a", RelayName: "Alpha", CountryCode: "JP", Label: "Tokyo, Japan", Latitude: 1, Longitude: 2},
		{CountryCode: "KR", Label: "Seoul, South Korea", Latitude: 3, Longitude: 4},
	}
	if err := adapter.SaveRecents(recents); err != nil {
		t.Fatalf("save recents: %v", err)
	}
	if got := adapter.LoadRecents(); len(got) != 2 || got[0] != recents[0] || got[1] != recents[1] {
		t.Fatalf("recents round trip = %+v", got)
	}
}

func TestConnectedStateCarriesTheRelayForTheSharedUI(t *testing.T) {
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit
	sink := &engineSink{s: s}

	legacy := "‮operator"
	sink.StateChanged(connectcore.State{
		Status:     connectcore.StatusConnected,
		RelayLabel: &legacy,
		Details: &connectcore.StateDetails{
			RelayID:       "relay_03a7324205e2b4d9",
			RelayName:     "  proud​ falcon ",
			RelayClass:    "foundation",
			LocationLabel: "Tokyo,‮ Japan",
		},
		Recents: []connectcore.RecentNode{
			{RelayID: "relay_03a7324205e2b4d9", RelayName: "relay_03a7324205e2b4d9", CountryCode: "JP", Label: "Tokyo, Japan"},
			{CountryCode: "KR", Label: "Seoul, South Korea"},
		},
	})
	last := cap.last()
	if last.RelayName == nil || *last.RelayName != "proud falcon" {
		t.Fatalf("relayName = %v, want the sanitized label", last.RelayName)
	}
	if last.RelayClass == nil || *last.RelayClass != "foundation" {
		t.Fatalf("relayClass = %v", last.RelayClass)
	}
	if last.RelayLabel == nil || *last.RelayLabel != "Tokyo, Japan" {
		t.Fatalf("relayLabel = %v, want the sanitized location", last.RelayLabel)
	}
	// A recent whose name is only the engine's id fallback gets the compact id.
	if got := last.Recents[0]; got.RelayID != "relay_03a7324205e2b4d9" || got.RelayName != "03a7324205e2" {
		t.Fatalf("pinned recent = %+v", got)
	}
	if got := last.Recents[1]; got.RelayID != "" || got.RelayName != "" {
		t.Fatalf("legacy recent gained a pin: %+v", got)
	}

	// An unlocated relay keeps the engine's own label, and the relay fields
	// disappear with the connection.
	sink.StateChanged(connectcore.State{
		Status:     connectcore.StatusConnected,
		RelayLabel: &legacy,
		Details:    &connectcore.StateDetails{RelayID: "relay_a", RelayName: "Alpha", RelayClass: "volunteer"},
	})
	if last = cap.last(); last.RelayLabel == nil || *last.RelayLabel != legacy {
		t.Fatalf("relayLabel = %v, want the engine label as fallback", last.RelayLabel)
	}
	sink.StateChanged(connectcore.State{Status: connectcore.StatusDisconnected, Details: &connectcore.StateDetails{}})
	if last = cap.last(); last.RelayName != nil || last.RelayClass != nil {
		t.Fatalf("relay fields outlived the connection: %+v", last)
	}
}

func TestFailedStateCarriesTheNoticeErrorCode(t *testing.T) {
	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit
	sink := &engineSink{s: s}
	code := func() string {
		if c := cap.last().LastErrorCode; c != nil {
			return *c
		}
		return ""
	}
	state := func(status connectcore.Status) { sink.StateChanged(connectcore.State{Status: status}) }

	// An operator disable: the refusal is followed directly by FAILED.
	state(connectcore.StatusConnecting)
	sink.Notice(connectcore.Notice{Kind: connectcore.NoticeAccessDenied})
	state(connectcore.StatusFailed)
	if code() != errorCodeAccessDenied {
		t.Fatalf("code = %q, want %q", code(), errorCodeAccessDenied)
	}
	// The polled state agrees with the emitted one until the next change.
	if got := s.nativeStateForTest(connectcore.State{Status: connectcore.StatusFailed}); got.LastErrorCode == nil || *got.LastErrorCode != errorCodeAccessDenied {
		t.Fatalf("polled code = %v", got.LastErrorCode)
	}
	// A later, unrelated failure is not labeled.
	state(connectcore.StatusConnecting)
	state(connectcore.StatusFailed)
	if code() != "" {
		t.Fatalf("stale code %q on an unrelated failure", code())
	}

	// An enrollment refusal survives the rest of its connect attempt…
	state(connectcore.StatusConnecting)
	sink.Notice(connectcore.Notice{Kind: connectcore.NoticeEnrollmentRefused})
	state(connectcore.StatusConnecting)
	state(connectcore.StatusFailed)
	if code() != errorCodeEnrollmentRefused {
		t.Fatalf("code = %q, want %q", code(), errorCodeEnrollmentRefused)
	}
	// …but not a successful connect followed by a different failure.
	state(connectcore.StatusConnecting)
	sink.Notice(connectcore.Notice{Kind: connectcore.NoticeEnrollmentRefused})
	state(connectcore.StatusConnected)
	state(connectcore.StatusFailed)
	if code() != "" {
		t.Fatalf("enrollment refusal leaked past a successful connect: %q", code())
	}
}

// nativeStateForTest is GetState's projection without a live engine.
func (s *Service) nativeStateForTest(state connectcore.State) NativeVpnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeStateLocked(state)
}
