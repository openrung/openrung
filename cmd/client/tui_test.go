package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/proxyconfig"
)

// fakeDriver records the engine commands the model issues, standing in for
// *connectcore.Engine behind the engineDriver seam.
type fakeDriver struct {
	mu          sync.Mutex
	connects    []connectcore.RelayTarget
	brokers     []string
	disconnects int
	directory   []connectcore.DirectoryRelay
	info        connectcore.ConnectionInfo
	infoOK      bool
	modes       []connectcore.Mode
	modeErr     error
}

func (d *fakeDriver) ConnectTarget(brokerURL string, target connectcore.RelayTarget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.brokers = append(d.brokers, brokerURL)
	d.connects = append(d.connects, target)
	return nil
}

func (d *fakeDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnects++
	return nil
}

func (d *fakeDriver) ActiveConnectionInfo() (connectcore.ConnectionInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.info, d.infoOK
}

func (d *fakeDriver) RankedDirectory(context.Context, string) ([]connectcore.DirectoryRelay, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.directory, nil
}

func (d *fakeDriver) LocalProxyPort() (int, error) { return 43210, nil }
func (d *fakeDriver) LocalProxyPortWarning() error { return nil }

func (d *fakeDriver) SetMode(mode connectcore.Mode) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.modeErr != nil {
		return d.modeErr
	}
	d.modes = append(d.modes, mode)
	return nil
}

func (d *fakeDriver) lastConnect(t *testing.T) (string, connectcore.RelayTarget) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.connects) == 0 {
		t.Fatal("no ConnectTarget call recorded")
	}
	return d.brokers[len(d.brokers)-1], d.connects[len(d.connects)-1]
}

func newTestModel(driver *fakeDriver) tuiModel {
	cfg := connectConfig{}
	cfg.BrokerURL = "http://broker.test"
	m := newTUIModel(driver, newLogRing(logRingCapacity), cfg)
	m.width, m.height = 80, 24
	return m
}

func keyMsg(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
}

// update runs one Update and executes the returned command synchronously so a
// test can observe the driver call it dispatched.
func update(t *testing.T, m tuiModel, msg tea.Msg) (tuiModel, tea.Msg) {
	t.Helper()
	next, cmd := m.Update(msg)
	model, ok := next.(tuiModel)
	if !ok {
		t.Fatalf("Update returned %T, want tuiModel", next)
	}
	if cmd == nil {
		return model, nil
	}
	return model, cmd()
}

// The c and x keys are retired: enter on the Relays list is the only connect
// control, and there is no pin left for x to clear. Both keys must be inert so
// stale muscle memory or a stale doc cannot silently half-work.
func TestRetiredConnectAndClearKeysAreInert(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.relays = []connectcore.DirectoryRelay{{Relay: brokerapi.RelayDescriptor{ID: "relay_a"}}}

	for _, stale := range []string{"c", "x"} {
		m, _ = update(t, m, keyMsg(stale))
		if len(driver.connects) != 0 {
			t.Fatalf("%q still connects: %+v", stale, driver.connects)
		}
		if m.view != viewRelays {
			t.Fatalf("%q moved the view to %d", stale, m.view)
		}
	}
}

func TestDisconnectKeyIssuesEngineDisconnect(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)

	_, _ = update(t, m, keyMsg("d"))

	if driver.disconnects != 1 {
		t.Fatalf("disconnects = %d, want 1", driver.disconnects)
	}
}

func TestRelaysEnterConnectsToTheHighlightedRelay(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.relays = []connectcore.DirectoryRelay{
		{Relay: brokerapi.RelayDescriptor{ID: "relay_a"}},
		{Relay: brokerapi.RelayDescriptor{ID: "relay_b"}},
	}

	// The cursor starts on Auto select, so the second relay is two steps down.
	m, _ = update(t, m, keyMsg("down"))
	m, _ = update(t, m, keyMsg("down"))
	m, _ = update(t, m, keyMsg("enter"))

	_, target := driver.lastConnect(t)
	if target.RelayID != "relay_b" {
		t.Fatalf("target = %+v, want the highlighted relay", target)
	}

	// The target lived only in that call: back on Auto select, the next connect
	// is ranked again — no pin lingers from the relay connect before it. (The
	// requested connect reports in, ends, and time passes first, or the
	// connect-spam guards would swallow the press.)
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusPreparing}))
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	m.now = m.now.Add(2 * time.Second)
	m, _ = update(t, m, keyMsg("up"))
	m, _ = update(t, m, keyMsg("up"))
	m, _ = update(t, m, keyMsg("enter"))
	if _, target := driver.lastConnect(t); target.Targeted() {
		t.Fatalf("Auto select connect still carries a target: %+v", target)
	}
}

// The status guard reads only the last PUBLISHED engine state, which arrives
// asynchronously: presses queued before that first state event must not each
// restart the ladder, and key repeats are capped at one connect per second.
func TestEnterSpamDispatchesOneConnect(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays

	for i := 0; i < 5; i++ {
		m, _ = update(t, m, keyMsg("enter"))
	}
	if len(driver.connects) != 1 {
		t.Fatalf("queued enters dispatched %d connects, want 1", len(driver.connects))
	}

	// A reconnect's teardown publishes the OLD session's Disconnected first,
	// and the new ladder may not report in for seconds. That event must not
	// unlatch enter — the reproduction was: connected → enter → old session's
	// Disconnected → wait past the throttle → enter → two ConnectTargets.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	m.now = m.now.Add(2 * time.Second)
	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 1 {
		t.Fatalf("old session's Disconnected unlatched enter: %d connects, want 1", len(driver.connects))
	}

	// Only the requested connect's own Preparing hands control back to the
	// status guard; once that attempt ends and a second passes, a fresh press
	// is a deliberate retry.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusPreparing}))
	m, _ = update(t, m, keyMsg("enter")) // in flight: the status guard holds
	if len(driver.connects) != 1 {
		t.Fatalf("enter during preparing dispatched %d connects, want 1", len(driver.connects))
	}
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	m.now = m.now.Add(2 * time.Second)
	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 2 {
		t.Fatalf("deliberate retry dispatched %d connects, want 2", len(driver.connects))
	}
}

// A connect torn down before it ever publishes preparing must not leave the
// pending latch stuck: an explicit d hands the latch back.
func TestDisconnectUnlatchesAPendingConnect(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays

	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 1 {
		t.Fatalf("connects = %d, want 1", len(driver.connects))
	}
	m, _ = update(t, m, keyMsg("d"))
	m.now = m.now.Add(2 * time.Second)
	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 2 {
		t.Fatalf("enter after an explicit disconnect stayed latched: %d connects", len(driver.connects))
	}
}

// Enter must not restart a connect that is already in flight (every
// ConnectTarget tears down and rebuilds the ladder, so mashing enter would
// keep it from converging), and enter on the relay the session is already on
// must not drop the live session — while Auto select stays live as an
// explicit "re-pick" even when connected.
func TestEnterGuardsInFlightAndCurrentRelay(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.relays = []connectcore.DirectoryRelay{{Relay: brokerapi.RelayDescriptor{ID: "relay_a"}}}

	for _, status := range []connectcore.Status{
		connectcore.StatusPreparing, connectcore.StatusConnecting, connectcore.StatusDisconnecting,
	} {
		m.state = connectcore.State{Status: status}
		m, _ = update(t, m, keyMsg("enter"))
		if len(driver.connects) != 0 {
			t.Fatalf("enter during %s issued a connect", status)
		}
	}

	m.state = connectcore.State{Status: connectcore.StatusConnected}
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{ID: "relay_a"}}
	m, _ = update(t, m, keyMsg("down")) // onto the connected relay's own row
	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 0 {
		t.Fatal("enter on the connected relay dropped and rebuilt the session")
	}
	m, _ = update(t, m, keyMsg("up")) // back to Auto select
	m, _ = update(t, m, keyMsg("enter"))
	if len(driver.connects) != 1 {
		t.Fatalf("Auto select while connected did not reconnect: %d connects", len(driver.connects))
	}
}

// -relay-country keeps the engine's country-scoped connect (and its
// intra-country failover) reachable from the binary now that the TUI holds no
// target state.
func TestRelayCountryFlagSeedsTheTarget(t *testing.T) {
	cfg, err := parseCommonFlags("check", []string{"-relay-country", "kr"})
	if err != nil {
		t.Fatal(err)
	}
	if target := cfg.target(); target.Country != "kr" || !target.Targeted() {
		t.Fatalf("target = %+v, want country kr", target)
	}
}

// Auto select — the list's first row, where the cursor starts — connects with
// no target at all: the broker's ranked pick. It works before the directory
// has ever loaded, since a ranked connect needs no list.
func TestRelaysAutoSelectConnectsRanked(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays

	_, _ = update(t, m, keyMsg("enter"))

	broker, target := driver.lastConnect(t)
	if broker != "http://broker.test" || target.Targeted() {
		t.Fatalf("ConnectTarget(%q, %+v), want the settings broker and no target", broker, target)
	}
}

func TestConnectedStateStampsSessionStartAndRendersStatus(t *testing.T) {
	driver := &fakeDriver{}
	driver.info = connectcore.ConnectionInfo{
		// Geo-bearing, like a real directory descriptor: the status bar single-
		// sources relay identity from the descriptor, so a bare one would read
		// "relay relay_a" and prove nothing about the label a user actually sees.
		Relay: brokerapi.RelayDescriptor{ID: "relay_a", NodeClass: brokerapi.NodeClassFoundation,
			RelayGeoLocation: brokerapi.RelayGeoLocation{City: "Seoul", Country: "South Korea", CountryCode: "kr"}},
		Transport: brokerapi.TransportDirect,
		ProxyPort: 43210,
	}
	driver.infoOK = true
	m := newTestModel(driver)

	label := "Seoul, South Korea"
	m, poll := update(t, m, engineStateMsg(connectcore.State{
		Status:     connectcore.StatusConnected,
		RelayLabel: &label,
	}))
	if m.connectedAt.IsZero() {
		t.Fatal("connectedAt not stamped on the transition into connected")
	}
	// The state transition polls the engine for path details; Init's proxy
	// resolution is replayed by hand since the test never starts the program.
	m, _ = update(t, m, poll)
	m, _ = update(t, m, m.resolveProxyCmd()())
	m.now = m.connectedAt.Add(90 * time.Second)

	// The status bar's detail track, not the rendered frame: at test width the
	// bar shows only a scrolling window into it. The duration is the one field
	// pinned to the rendered bar, checked separately below.
	view := m.statusDetail()
	for _, want := range []string{"direct", "127.0.0.1:43210"} {
		if !strings.Contains(view, want) {
			t.Fatalf("status detail missing %q:\n%s", want, view)
		}
	}
	// The relay, its country, and the duration are the pin's, not the detail's;
	// the connection state itself is the bar's color.
	pin := m.statusPin(0)
	for _, want := range []string{label, "KR", "00:01:30"} {
		if !strings.Contains(pin, want) {
			t.Fatalf("status pin missing %q:\n%q", want, pin)
		}
	}
}

func TestSettingsEditCommitsBrokerURL(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings // cursor starts on the broker field

	m, _ = update(t, m, keyMsg("enter"))
	if !m.settings.editing {
		t.Fatal("enter did not begin editing")
	}
	m.settings.input.SetValue("http://other.test")
	m, _ = update(t, m, keyMsg("enter"))
	if m.settings.editing || m.settings.brokerURL != "http://other.test" {
		t.Fatalf("broker = %q (editing=%t), want the committed value", m.settings.brokerURL, m.settings.editing)
	}

	// Editing captures printable keys: 'q' must not quit, 'c' must not connect.
	m, _ = update(t, m, keyMsg("enter"))
	m, msg := update(t, m, keyMsg("q"))
	if _, quit := msg.(tea.QuitMsg); quit {
		t.Fatal("q while editing quit the program")
	}
	if len(driver.connects) != 0 {
		t.Fatal("printable key while editing reached a global shortcut")
	}
}

func TestModeFieldTogglesCaptureModeThroughTheEngine(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings

	m, msg := update(t, m, keyMsg("down")) // onto the mode field
	m, msg = update(t, m, keyMsg("enter"))
	if m.settings.editing {
		t.Fatal("mode field must not open a text editor")
	}
	// The engine owns the mode: the view adopts it only from the reply.
	if m.settings.mode != connectcore.ModeProxy {
		t.Fatalf("mode changed before the engine answered: %v", m.settings.mode)
	}
	if got := driver.modes; len(got) != 1 || got[0] != connectcore.ModeTUN {
		t.Fatalf("SetMode calls = %v; want one ModeTUN", got)
	}
	m, _ = update(t, m, msg)
	if m.settings.mode != connectcore.ModeTUN {
		t.Fatalf("mode = %v after a clean SetMode; want TUN", m.settings.mode)
	}
	if m.settings.note.kind == noteNone {
		t.Fatal("switching mode said nothing about what changes")
	}

	// Toggling again goes back to proxy.
	m, msg = update(t, m, keyMsg("enter"))
	m, _ = update(t, m, msg)
	if m.settings.mode != connectcore.ModeProxy {
		t.Fatalf("mode = %v after a second toggle; want proxy", m.settings.mode)
	}
}

// A live session keeps the mode it started with, so the engine's refusal has
// to reach the user instead of the view drifting out of sync with it.
func TestModeToggleRefusalKeepsTheEngineMode(t *testing.T) {
	driver := &fakeDriver{modeErr: errors.New("disconnect before changing the capture mode")}
	m := newTestModel(driver)
	m.view = viewSettings

	m, _ = update(t, m, keyMsg("down"))
	m, msg := update(t, m, keyMsg("enter"))
	m, _ = update(t, m, msg)
	if m.settings.mode != connectcore.ModeProxy {
		t.Fatalf("mode = %v after a refused SetMode; want the engine's proxy mode", m.settings.mode)
	}
	if !strings.Contains(m.settings.note.text, "disconnect before") {
		t.Fatalf("note = %+v; want the engine's refusal", m.settings.note)
	}
}

// TUN mode has no local endpoint, so the client must not resolve (and persist)
// a stable proxy port for a listener that never opens.
func TestTUNModeSkipsProxyPortResolution(t *testing.T) {
	cfg := connectConfig{TUN: true}
	m := newTUIModel(&fakeDriver{}, newLogRing(logRingCapacity), cfg)
	if m.settings.mode != connectcore.ModeTUN {
		t.Fatalf("mode = %v; want TUN from --tun", m.settings.mode)
	}
	m.width, m.height = 80, 24
	// Read the status bar's detail track rather than the frame: at 80 cells the
	// bar shows a window into it, so the capture field may be scrolled out.
	if detail := m.statusDetail(); !strings.Contains(detail, "TUN — whole device") {
		t.Fatalf("status bar did not report TUN capture:\n%s", detail)
	}
	if strings.Contains(m.statusDetail(), "resolving…") {
		t.Fatal("TUN mode still advertises a local proxy endpoint")
	}
}

func TestQuitKeyQuits(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)

	_, msg := update(t, m, keyMsg("q"))
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("q returned %T, want tea.QuitMsg", msg)
	}
}

func TestLegacyFlagWarningsNameQuietKnobs(t *testing.T) {
	cfg := connectConfig{ConfigOut: "x.json"}
	cfg.Limit = 9
	cfg.MTU = 1280
	cfg.Family = "ipv6"
	warnings := legacyFlagWarnings(cfg)
	if len(warnings) != 4 {
		t.Fatalf("warnings = %v, want one per ignored flag", warnings)
	}

	cfg = connectConfig{}
	cfg.Limit = defaultRelayLimit
	cfg.Family = defaultRelayFamily
	if warnings := legacyFlagWarnings(cfg); len(warnings) != 0 {
		t.Fatalf("default flags warned: %v", warnings)
	}
}

func TestLogRingCapsAndCoalesces(t *testing.T) {
	ring := newLogRing(3)
	if _, dirty := ring.snapshotIfDirty(); dirty {
		t.Fatal("empty ring reported dirty")
	}
	for _, line := range []string{"a", "b", "c", "d"} {
		ring.push(line)
	}
	lines, dirty := ring.snapshotIfDirty()
	if !dirty || strings.Join(lines, " ") != "b c d" {
		t.Fatalf("snapshot = %v (dirty=%t), want the capped tail", lines, dirty)
	}
	if _, dirty := ring.snapshotIfDirty(); dirty {
		t.Fatal("ring stayed dirty after a snapshot")
	}
}

func TestFailoverKeepsSessionStart(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)

	label := "Tokyo, Japan"
	connected := connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
	m, _ = update(t, m, engineStateMsg(connected))
	started := m.connectedAt

	// An automatic failover re-promotes through connecting → connected without
	// ending the session; the stamp must survive it.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusConnecting, RelayLabel: &label}))
	m, _ = update(t, m, engineStateMsg(connected))
	if !m.connectedAt.Equal(started) {
		t.Fatalf("connectedAt = %v, want the original stamp %v across a failover", m.connectedAt, started)
	}

	// A disconnect ends the session; the next connect stamps fresh.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	if !m.connectedAt.IsZero() {
		t.Fatalf("connectedAt = %v, want cleared after disconnect", m.connectedAt)
	}
}

func TestCtrlCQuitsWhileEditingSettings(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings

	m, _ = update(t, m, keyMsg("enter")) // begin editing the broker field
	_, msg := update(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c while editing returned %T, want tea.QuitMsg", msg)
	}
}

func TestRelaysWindowFollowsCursor(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.refreshing = false
	m.height = 12 // body of 7 rows; the brand mark reserves 4, header 1 → 2 list rows
	for i := 0; i < 30; i++ {
		m.relays = append(m.relays, connectcore.DirectoryRelay{
			Relay: brokerapi.RelayDescriptor{ID: fmt.Sprintf("relay_%02d", i), Label: fmt.Sprintf("node-%02d", i)},
		})
	}

	// The last relay sits at cursor len(relays): index 0 is the Auto row.
	m.relayCursor = len(m.relays)
	view := m.View()
	if !strings.Contains(view, "node-29") {
		t.Fatalf("last relay not visible with the cursor on it:\n%s", view)
	}
	if strings.Contains(view, "node-00") || strings.Contains(view, m.tr().autoSelect) {
		t.Fatalf("head of the list still rendered while the cursor sits at the tail:\n%s", view)
	}

	m.relayCursor = 0
	view = m.View()
	if !strings.Contains(view, m.tr().autoSelect) || !strings.Contains(view, "node-00") || strings.Contains(view, "node-29") {
		t.Fatalf("window did not follow the cursor back to the head:\n%s", view)
	}
}

func TestEngineNoticesDriveActivityAndHealthRows(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)

	label := "Tokyo, Japan"
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}))

	m, _ = update(t, m, engineNoticeMsg(connectcore.Notice{
		Kind:        connectcore.NoticeFailoverStarted,
		FromRelayID: "relay_a",
		Reason:      "tunnel process exited unexpectedly",
	}))
	m, _ = update(t, m, engineNoticeMsg(connectcore.Notice{
		Kind:      connectcore.NoticeHealthProbe,
		Failures:  2,
		Threshold: 3,
	}))

	// The bar's detail track: at test width the rendered bar is a window into it.
	view := m.statusDetail()
	for _, want := range []string{"failover: relay relay_a lost", "tunnel process exited unexpectedly", "2/3 probes failed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("status detail missing %q:\n%s", want, view)
		}
	}

	// A WSS fallback replaces the activity line and names the front.
	m, _ = update(t, m, engineNoticeMsg(connectcore.Notice{
		Kind:    connectcore.NoticeWSSFallback,
		RelayID: "relay_a",
		FrontID: "front-a",
		Reason:  "direct TCP blocked",
	}))
	view = m.statusDetail()
	if !strings.Contains(view, "WSS fallback: relay relay_a via front front-a") {
		t.Fatalf("status detail missing the WSS fallback activity:\n%s", view)
	}

	// Disconnecting ends the session: health state goes with it, the last
	// activity stays visible so the outcome remains explainable.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	if m.health.Kind != "" {
		t.Fatalf("health notice survived disconnect: %+v", m.health)
	}
	if m.activity.Kind == "" {
		t.Fatal("activity notice cleared on disconnect")
	}

	// A genuinely new connection (preparing — a failover recovery never passes
	// through it) must not display the previous session's events.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusPreparing}))
	if m.activity.Kind != "" {
		t.Fatalf("old session's activity leaked into the new connection: %+v", m.activity)
	}
}

func TestShellHelperActionRequiresConnection(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings
	m.shellHelper = func() (proxyconfig.Info, error) {
		return proxyconfig.Info{
			EnableCommand:  ". '/home/u/.config/openrung/proxy-env.sh' && openrung_proxy_on",
			DisableCommand: "openrung_proxy_off",
		}, nil
	}
	for i := 0; i < int(fieldShellHelper); i++ {
		m, _ = update(t, m, keyMsg("down"))
	}

	// Disconnected: the action explains itself instead of generating commands.
	m, msg := update(t, m, keyMsg("enter"))
	if msg != nil {
		t.Fatalf("disconnected shell-helper action dispatched %T", msg)
	}
	if m.settings.note.kind == noteNone || m.settings.shellOK {
		t.Fatalf("disconnected action note=%+v shellOK=%t", m.settings.note, m.settings.shellOK)
	}

	// Connected: enter generates and renders both copyable commands.
	label := "Tokyo, Japan"
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}))
	m, msg = update(t, m, keyMsg("enter"))
	m, _ = update(t, m, msg)
	if !m.settings.shellOK {
		t.Fatalf("shell helper not generated: %+v", m.settings)
	}
	view := m.View()
	for _, want := range []string{"openrung_proxy_on", "openrung_proxy_off", "run the restore command"} {
		if !strings.Contains(view, want) {
			t.Fatalf("settings view missing %q:\n%s", want, view)
		}
	}

	// After disconnect the enable command would point at a dead proxy, so it
	// disappears; the restore command stays — that is exactly when a shell
	// still carrying our variables needs it.
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusDisconnected}))
	view = m.View()
	if strings.Contains(view, "openrung_proxy_on") {
		t.Fatalf("enable command still shown while disconnected:\n%s", view)
	}
	if !strings.Contains(view, "openrung_proxy_off") {
		t.Fatalf("restore command hidden while disconnected:\n%s", view)
	}
}

func TestShellHelperErrorIsSurfaced(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings
	m.shellHelper = func() (proxyconfig.Info, error) {
		return proxyconfig.Info{}, errors.New("proxy configuration directory is unavailable")
	}
	label := "Tokyo, Japan"
	m, _ = update(t, m, engineStateMsg(connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}))
	for i := 0; i < int(fieldShellHelper); i++ {
		m, _ = update(t, m, keyMsg("down"))
	}

	m, msg := update(t, m, keyMsg("enter"))
	m, _ = update(t, m, msg)
	if m.settings.shellOK || m.settings.shellErr.kind == noteNone {
		t.Fatalf("shell error not surfaced: %+v", m.settings)
	}
	if !strings.Contains(m.View(), "proxy configuration directory is unavailable") {
		t.Fatalf("settings view missing the shell error:\n%s", m.View())
	}
}
