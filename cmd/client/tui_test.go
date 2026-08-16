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

	"openrung/internal/connectcore"
	"openrung/internal/proxyconfig"
	"openrung/internal/relay"
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

func TestConnectKeyIssuesEngineConnectWithSettingsTarget(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.settings.target = connectcore.RelayTarget{Country: "kr"}

	_, _ = update(t, m, keyMsg("c"))

	broker, target := driver.lastConnect(t)
	if broker != "http://broker.test" || target.Country != "kr" {
		t.Fatalf("ConnectTarget(%q, %+v), want the settings broker and target", broker, target)
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

func TestRelaysEnterPinsSelectionAndConnects(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.relays = []connectcore.DirectoryRelay{
		{Relay: relay.Descriptor{ID: "relay_a"}},
		{Relay: relay.Descriptor{ID: "relay_b"}},
	}

	m, _ = update(t, m, keyMsg("down"))
	m, _ = update(t, m, keyMsg("enter"))

	_, target := driver.lastConnect(t)
	if target.RelayID != "relay_b" {
		t.Fatalf("target = %+v, want the highlighted relay pinned", target)
	}
	if m.settings.target.RelayID != "relay_b" {
		t.Fatalf("settings target = %+v, want the pinned relay retained", m.settings.target)
	}

	// x clears the pin so the next connect is automatic again.
	m, _ = update(t, m, keyMsg("x"))
	if m.settings.target.Targeted() {
		t.Fatalf("settings target = %+v, want cleared", m.settings.target)
	}
}

func TestConnectedStateStampsSessionStartAndRendersStatus(t *testing.T) {
	driver := &fakeDriver{}
	driver.info = connectcore.ConnectionInfo{
		Relay:     relay.Descriptor{ID: "relay_a", NodeClass: relay.NodeClassFoundation, GeoLocation: relay.GeoLocation{CountryCode: "kr"}},
		Transport: relay.TransportDirect,
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

	view := m.View()
	for _, want := range []string{"connected", label, "direct", "KR", "00:01:30", "127.0.0.1:43210"} {
		if !strings.Contains(view, want) {
			t.Fatalf("status view missing %q:\n%s", want, view)
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

func TestModeFieldExplainsProxyOnly(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewSettings

	m, _ = update(t, m, keyMsg("down")) // onto the mode field
	m, _ = update(t, m, keyMsg("enter"))
	if m.settings.editing {
		t.Fatal("mode field must not open a text editor")
	}
	if m.settings.note == "" {
		t.Fatal("mode field did not explain why TUN is unavailable")
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
	m.height = 12 // body of 8 rows: notice-free header + 7 relay rows visible
	for i := 0; i < 30; i++ {
		m.relays = append(m.relays, connectcore.DirectoryRelay{
			Relay: relay.Descriptor{ID: fmt.Sprintf("relay_%02d", i), Label: fmt.Sprintf("node-%02d", i)},
		})
	}

	m.relayCursor = len(m.relays) - 1
	view := m.View()
	if !strings.Contains(view, "node-29") {
		t.Fatalf("last relay not visible with the cursor on it:\n%s", view)
	}
	if strings.Contains(view, "node-00") {
		t.Fatalf("head of the list still rendered while the cursor sits at the tail:\n%s", view)
	}

	m.relayCursor = 0
	view = m.View()
	if !strings.Contains(view, "node-00") || strings.Contains(view, "node-29") {
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

	view := m.View()
	for _, want := range []string{"failover: relay relay_a lost", "tunnel process exited unexpectedly", "2/3 probes failed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("status view missing %q:\n%s", want, view)
		}
	}

	// A WSS fallback replaces the activity line and names the front.
	m, _ = update(t, m, engineNoticeMsg(connectcore.Notice{
		Kind:    connectcore.NoticeWSSFallback,
		RelayID: "relay_a",
		FrontID: "front-a",
		Reason:  "direct TCP blocked",
	}))
	view = m.View()
	if !strings.Contains(view, "WSS fallback: relay relay_a via front front-a") {
		t.Fatalf("status view missing the WSS fallback activity:\n%s", view)
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

func TestRecentsRenderInRelaysView(t *testing.T) {
	driver := &fakeDriver{}
	m := newTestModel(driver)
	m.view = viewRelays
	m.refreshing = false
	m.state.Recents = []connectcore.RecentNode{
		{CountryCode: "KR", Label: "Seoul, South Korea"},
		{CountryCode: "JP", Label: "Tokyo, Japan"},
	}

	view := m.View()
	if !strings.Contains(view, "Seoul, South Korea · Tokyo, Japan") {
		t.Fatalf("relays view missing the recents row:\n%s", view)
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
	if m.settings.note == "" || m.settings.shellOK {
		t.Fatalf("disconnected action note=%q shellOK=%t", m.settings.note, m.settings.shellOK)
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
	if m.settings.shellOK || m.settings.shellErr == "" {
		t.Fatalf("shell error not surfaced: %+v", m.settings)
	}
	if !strings.Contains(m.View(), "proxy configuration directory is unavailable") {
		t.Fatalf("settings view missing the shell error:\n%s", m.View())
	}
}
