package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"openrung/internal/connectcore"
)

// The interactive client is a pure view over the shared connection engine
// (docs/adr/001 Track B): engine events arrive as Bubble Tea messages through
// tuiSink, and engineDriver is the only command surface out. No brokerapi,
// sing-box, WSS, or punch calls live in this layer.

// engineDriver is the exact engine surface the TUI consumes. Every call that
// can take an engine lock runs inside a tea.Cmd goroutine, never in Update:
// the sink delivers events synchronously under the engine's state lock, so an
// Update blocked on the engine while the sink blocks on the event loop would
// deadlock.
type engineDriver interface {
	ConnectTarget(brokerURL string, target connectcore.RelayTarget) error
	Disconnect() error
	ActiveConnectionInfo() (connectcore.ConnectionInfo, bool)
	RankedDirectory(ctx context.Context, brokerURL string) ([]connectcore.DirectoryRelay, error)
	LocalProxyPort() (int, error)
	LocalProxyPortWarning() error
}

// tuiSink forwards engine events into the Bubble Tea loop. Status changes are
// sent immediately — transitions are rare and the UI must not lag them — while
// log lines only land in the shared ring, flushed by the model's tick, so a
// chatty sing-box burst cannot flood the event loop (the same coalescing the
// desktop webview emitter applies).
type tuiSink struct {
	ring *logRing

	mu sync.Mutex
	p  *tea.Program
}

func (s *tuiSink) attach(p *tea.Program) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

func (s *tuiSink) StateChanged(state connectcore.State) {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p != nil {
		p.Send(engineStateMsg(state))
	}
}

func (s *tuiSink) Log(entry connectcore.LogEntry) {
	s.ring.push(stampLog(entry.Time, entry.Line))
}

func stampLog(at time.Time, line string) string {
	return "[" + at.Format("15:04:05") + "] " + line
}

// runTUI wires the engine host and hands the terminal to Bubble Tea. The
// deferred engine.Stop is the graceful-teardown guarantee: it runs on quit and
// on a panic unwinding through Run (Bubble Tea restores the terminal before
// re-panicking), tearing the tunnel down, restoring the OS proxy, and ending
// and flushing the telemetry session.
func runTUI(cfg connectConfig) error {
	ring := newLogRing(logRingCapacity)
	sink := &tuiSink{ring: ring}
	host := newEngineHost(sink, cfg.SingBoxPath)
	engine := host.engine
	engine.PunchEnabled = cfg.PunchEnabled
	engine.PunchURL = cfg.PunchURL
	engine.PunchInsecure = cfg.PunchInsecure

	for _, warning := range legacyFlagWarnings(cfg) {
		ring.push(stampLog(time.Now(), "warning: "+warning))
	}

	// Crash recovery and persisted recents; runs before the event loop exists,
	// so its log lines are already in the ring for the first flush.
	engine.Start()
	defer engine.Stop()

	p := tea.NewProgram(newTUIModel(engine, ring, cfg), tea.WithAltScreen())
	sink.attach(p)
	_, err := p.Run()
	return err
}

// ---- messages ----

type engineStateMsg connectcore.State

type tickMsg time.Time

// pollMsg carries the engine reads the tick triggers: the promoted candidate's
// path details for the Status view.
type pollMsg struct {
	info   connectcore.ConnectionInfo
	infoOK bool
}

type directoryMsg struct {
	relays []connectcore.DirectoryRelay
	err    error
}

type proxyInfoMsg struct {
	endpoint string
	warn     string
	err      string
}

type connectIssuedMsg struct{ err error }

// ---- model ----

type viewID int

const (
	viewStatus viewID = iota
	viewRelays
	viewLogs
	viewSettings
	viewCount
)

type settingsFieldID int

const (
	fieldBroker settingsFieldID = iota
	fieldMode
	fieldRelayID
	fieldRelayLabel
	fieldCountry
	settingsFieldCount
)

type settingsState struct {
	brokerURL string
	target    connectcore.RelayTarget

	cursor  settingsFieldID
	editing bool
	input   textinput.Model
	note    string
}

type tuiModel struct {
	driver engineDriver
	ring   *logRing

	view          viewID
	width, height int

	state       connectcore.State
	info        connectcore.ConnectionInfo
	infoOK      bool
	connectedAt time.Time
	now         time.Time

	proxyEndpoint string
	proxyWarn     string
	proxyErr      string

	logLines []string
	logView  viewport.Model
	logReady bool

	relays      []connectcore.DirectoryRelay
	relayErr    string
	relayCursor int
	refreshing  bool

	settings settingsState
}

const tuiTickInterval = 200 * time.Millisecond

func newTUIModel(driver engineDriver, ring *logRing, cfg connectConfig) tuiModel {
	input := textinput.New()
	input.CharLimit = 256
	return tuiModel{
		driver: driver,
		ring:   ring,
		now:    time.Now(),
		state:  connectcore.State{Status: connectcore.StatusDisconnected},
		settings: settingsState{
			brokerURL: cfg.BrokerURL,
			target:    cfg.target(),
			input:     input,
		},
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.tickCmd(), m.refreshDirectoryCmd(), m.resolveProxyCmd())
}

// ---- commands (every engine call happens here, off the event loop) ----

func (m tuiModel) tickCmd() tea.Cmd {
	return tea.Tick(tuiTickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) pollCmd() tea.Cmd {
	driver := m.driver
	return func() tea.Msg {
		info, ok := driver.ActiveConnectionInfo()
		return pollMsg{info: info, infoOK: ok}
	}
}

func (m tuiModel) refreshDirectoryCmd() tea.Cmd {
	driver, broker := m.driver, m.settings.brokerURL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		relays, err := driver.RankedDirectory(ctx, broker)
		return directoryMsg{relays: relays, err: err}
	}
}

func (m tuiModel) resolveProxyCmd() tea.Cmd {
	driver := m.driver
	return func() tea.Msg {
		port, err := driver.LocalProxyPort()
		if err != nil {
			return proxyInfoMsg{err: err.Error()}
		}
		msg := proxyInfoMsg{endpoint: connectcore.ProxyHost + ":" + strconv.Itoa(port)}
		if warning := driver.LocalProxyPortWarning(); warning != nil {
			msg.warn = warning.Error()
		}
		return msg
	}
}

func (m tuiModel) connectCmd() tea.Cmd {
	driver, broker, target := m.driver, m.settings.brokerURL, m.settings.target
	return func() tea.Msg {
		return connectIssuedMsg{err: driver.ConnectTarget(broker, target)}
	}
}

func (m tuiModel) disconnectCmd() tea.Cmd {
	driver := m.driver
	return func() tea.Msg {
		_ = driver.Disconnect()
		return nil
	}
}

// ---- update ----

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeLogView()
		return m, nil

	case engineStateMsg:
		prev := m.state.Status
		m.state = connectcore.State(msg)
		if m.state.Status == connectcore.StatusConnected && prev != connectcore.StatusConnected {
			m.connectedAt = time.Now()
		}
		return m, m.pollCmd()

	case tickMsg:
		m.now = time.Time(msg)
		if lines, ok := m.ring.snapshotIfDirty(); ok {
			m.setLogLines(lines)
		}
		return m, tea.Batch(m.tickCmd(), m.pollCmd())

	case pollMsg:
		m.info, m.infoOK = msg.info, msg.infoOK
		return m, nil

	case directoryMsg:
		m.refreshing = false
		if msg.err != nil {
			m.relayErr = msg.err.Error()
			return m, nil
		}
		m.relayErr = ""
		m.relays = msg.relays
		if m.relayCursor >= len(m.relays) {
			m.relayCursor = max(0, len(m.relays)-1)
		}
		return m, nil

	case proxyInfoMsg:
		m.proxyEndpoint, m.proxyWarn, m.proxyErr = msg.endpoint, msg.warn, msg.err
		return m, nil

	case connectIssuedMsg:
		if msg.err != nil {
			m.ring.push(stampLog(time.Now(), "connect failed to start: "+msg.err.Error()))
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A focused settings editor captures everything except its commit/cancel
	// keys, so typed characters never trigger global shortcuts.
	if m.view == viewSettings && m.settings.editing {
		return m.handleSettingsEditKey(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		// Teardown runs after the event loop exits (runTUI's deferred Stop), so
		// quitting mid-session still disconnects, restores the OS proxy, and
		// flushes telemetry before the process ends.
		return m, tea.Quit
	case "1":
		m.view = viewStatus
		return m, nil
	case "2":
		m.view = viewRelays
		return m, nil
	case "3":
		m.view = viewLogs
		return m, nil
	case "4":
		m.view = viewSettings
		return m, nil
	case "tab":
		m.view = (m.view + 1) % viewCount
		return m, nil
	case "shift+tab":
		m.view = (m.view + viewCount - 1) % viewCount
		return m, nil
	case "c":
		return m, m.connectCmd()
	case "d":
		return m, m.disconnectCmd()
	case "r":
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		return m, m.refreshDirectoryCmd()
	}

	switch m.view {
	case viewRelays:
		return m.handleRelaysKey(msg)
	case viewLogs:
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		return m, cmd
	case viewSettings:
		return m.handleSettingsKey(msg)
	}
	return m, nil
}

func (m tuiModel) handleRelaysKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.relayCursor > 0 {
			m.relayCursor--
		}
	case "down", "j":
		if m.relayCursor < len(m.relays)-1 {
			m.relayCursor++
		}
	case "enter":
		// Manual relay selection, like the desktop map's targeting: pin the
		// highlighted relay and connect to it.
		if m.relayCursor < len(m.relays) {
			m.settings.target = connectcore.RelayTarget{RelayID: m.relays[m.relayCursor].Relay.ID}
			return m, m.connectCmd()
		}
	case "x":
		m.settings.target = connectcore.RelayTarget{}
	}
	return m, nil
}

func (m tuiModel) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.settings.cursor > 0 {
			m.settings.cursor--
		}
	case "down", "j":
		if m.settings.cursor < settingsFieldCount-1 {
			m.settings.cursor++
		}
	case "enter":
		if m.settings.cursor == fieldMode {
			// The engine runs proxy mode only until ADR-001 Track B3 wires the
			// elevation hook; surface why the toggle does nothing yet.
			m.settings.note = "TUN mode arrives in a later release; proxy mode needs no privileges"
			return m, nil
		}
		m.settings.editing = true
		m.settings.note = ""
		m.settings.input.SetValue(m.settingsFieldValue(m.settings.cursor))
		m.settings.input.CursorEnd()
		return m, m.settings.input.Focus()
	}
	return m, nil
}

func (m tuiModel) handleSettingsEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.applySettingsField(m.settings.cursor, strings.TrimSpace(m.settings.input.Value()))
		m.settings.editing = false
		m.settings.input.Blur()
		return m, nil
	case "esc":
		m.settings.editing = false
		m.settings.input.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.settings.input, cmd = m.settings.input.Update(msg)
	return m, cmd
}

func (m tuiModel) settingsFieldValue(field settingsFieldID) string {
	switch field {
	case fieldBroker:
		return m.settings.brokerURL
	case fieldRelayID:
		return m.settings.target.RelayID
	case fieldRelayLabel:
		return m.settings.target.Label
	case fieldCountry:
		return m.settings.target.Country
	}
	return ""
}

func (m *tuiModel) applySettingsField(field settingsFieldID, value string) {
	switch field {
	case fieldBroker:
		m.settings.brokerURL = value
	case fieldRelayID:
		m.settings.target.RelayID = value
	case fieldRelayLabel:
		m.settings.target.Label = value
	case fieldCountry:
		m.settings.target.Country = value
	}
}

func (m *tuiModel) setLogLines(lines []string) {
	m.logLines = lines
	if !m.logReady {
		return
	}
	follow := m.logView.AtBottom()
	m.logView.SetContent(strings.Join(lines, "\n"))
	if follow {
		m.logView.GotoBottom()
	}
}

func (m *tuiModel) resizeLogView() {
	bodyHeight := m.bodyHeight()
	if bodyHeight < 1 || m.width < 1 {
		return
	}
	if !m.logReady {
		m.logView = viewport.New(m.width, bodyHeight)
		m.logReady = true
		m.logView.SetContent(strings.Join(m.logLines, "\n"))
		m.logView.GotoBottom()
		return
	}
	m.logView.Width = m.width
	m.logView.Height = bodyHeight
}
