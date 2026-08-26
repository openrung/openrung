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

	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/proxyconfig"
)

// The interactive client is a pure view over the shared connection engine
// (ADR-001 Track B): engine events arrive as Bubble Tea messages through
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
	SetMode(mode connectcore.Mode) error
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

// Notice forwards the engine's typed mid-flow events (failover, WSS fallback,
// punch outcome, health probes) like a state change: immediately, since each
// one is rare and drives a dedicated Status-view row.
func (s *tuiSink) Notice(notice connectcore.Notice) {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p != nil {
		p.Send(engineNoticeMsg(notice))
	}
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
	host := newEngineHost(sink, cfg.SingBoxPath, cfg.SingBoxExternal)
	engine := host.engine
	engine.PunchEnabled = cfg.PunchEnabled
	engine.PunchURL = cfg.PunchURL
	engine.PunchInsecure = cfg.PunchInsecure
	engine.TunnelMTU = cfg.MTU
	// Nothing is connected yet, so this cannot fail; the Settings toggle uses
	// the same call and does surface a refusal.
	if err := engine.SetMode(cfg.mode()); err != nil {
		return err
	}

	for _, warning := range legacyFlagWarnings(cfg) {
		ring.push(stampLog(time.Now(), "warning: "+warning))
	}

	// Crash recovery and persisted recents; runs before the event loop exists,
	// so its log lines are already in the ring for the first flush.
	engine.Start()
	defer engine.Stop()

	model := newTUIModel(engine, ring, cfg)
	// Start ran above, so the persisted recents are already in the engine
	// state; seed the model with them (events only arrive on changes).
	model.state = engine.State()
	model.shellHelper = host.shellProxyHelper

	p := tea.NewProgram(model, tea.WithAltScreen())
	sink.attach(p)
	_, err := p.Run()
	return err
}

// ---- messages ----

type engineStateMsg connectcore.State

type engineNoticeMsg connectcore.Notice

// shellHelperMsg carries the generated shell-helper commands (or why they are
// unavailable) back from the host closure. unavailable means this build has
// no shell integration — a fixed notice, translated at render time.
type shellHelperMsg struct {
	info        proxyconfig.Info
	err         string
	unavailable bool
}

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

// modeSetMsg carries the outcome of a capture-mode change: the engine either
// adopted the mode or said why it would not.
type modeSetMsg struct {
	mode    connectcore.Mode
	applied bool
	err     string
}

// ---- model ----

type viewID int

// There is no Status view: its rows are the statusFooter, a permanent bar
// above the key help, so connection state is visible from every view instead
// of only the one the user happened to be on.
const (
	viewRelays viewID = iota
	viewLogs
	viewSettings
	viewCount
)

type settingsFieldID int

// The target relay is not a settings field: it is pinned from the Relays view
// (enter targets the highlighted relay, x clears) or by CLI flags, and the
// Status view's Target row shows what is pinned.
const (
	fieldBroker settingsFieldID = iota
	fieldMode
	fieldShellHelper
	settingsFieldCount
)

// A settings notice is stored as a kind and rendered through the active
// language at draw time: storing translated text would survive a language
// cycle and leave mixed-language UI.
type noteKind int

const (
	noteNone noteKind = iota
	// noteText is engine- or helper-provided text, shown verbatim (the
	// engine speaks English regardless of the UI language).
	noteText
	noteModeTUN
	noteModeProxy
	noteShellTUN
	noteShellDisconnected
	noteShellUnavailable
)

type settingsNote struct {
	kind noteKind
	text string // noteText only
}

type settingsState struct {
	brokerURL string
	target    connectcore.RelayTarget
	// mode mirrors the engine's capture mode. The engine owns it (a live
	// session keeps the mode it started with), so the view only ever shows
	// what a SetMode call confirmed.
	mode connectcore.Mode

	cursor  settingsFieldID
	editing bool
	input   textinput.Model
	note    settingsNote

	// Generated shell-helper commands (the desktop Settings "LOCAL PROXY"
	// section); populated by the shell-helper action while connected.
	shell    proxyconfig.Info
	shellOK  bool
	shellErr settingsNote
}

type tuiModel struct {
	driver engineDriver
	ring   *logRing

	// shellHelper generates the copyable shell proxy commands (host wiring,
	// not an engine call); nil means shell integration is unavailable.
	shellHelper func() (proxyconfig.Info, error)

	view          viewID
	lang          language
	width, height int

	state       connectcore.State
	info        connectcore.ConnectionInfo
	infoOK      bool
	connectedAt time.Time
	now         time.Time
	startedAt   time.Time

	// Latest typed engine notices: activity is the last connection event
	// (failover, WSS fallback, punch, ticket retry), health the last
	// mid-session probe sweep.
	activity   connectcore.Notice
	activityAt time.Time
	health     connectcore.Notice

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
	now := time.Now()
	return tuiModel{
		driver:    driver,
		ring:      ring,
		now:       now,
		startedAt: now,
		state:     connectcore.State{Status: connectcore.StatusDisconnected},
		// Init issues the first directory refresh, so mark it in flight: the
		// Relays view says "refreshing" instead of inviting an r that would
		// race it.
		refreshing: true,
		settings: settingsState{
			brokerURL: cfg.BrokerURL,
			target:    cfg.target(),
			mode:      cfg.mode(),
			input:     input,
		},
	}
}

func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.tickCmd(), m.refreshDirectoryCmd()}
	// TUN mode binds no local endpoint, so it neither needs nor persists the
	// stable proxy port; resolving one would pick a port for a listener that
	// never opens.
	if m.settings.mode == connectcore.ModeProxy {
		cmds = append(cmds, m.resolveProxyCmd())
	}
	return tea.Batch(cmds...)
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
		msg := proxyInfoMsg{endpoint: proxyconfig.Host + ":" + strconv.Itoa(port)}
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

// setModeCmd asks the engine to change capture mode. The engine refuses while
// a connection is live, so the view adopts the new mode only once the call
// came back clean.
func (m tuiModel) setModeCmd(mode connectcore.Mode) tea.Cmd {
	driver := m.driver
	return func() tea.Msg {
		if err := driver.SetMode(mode); err != nil {
			return modeSetMsg{err: err.Error()}
		}
		return modeSetMsg{mode: mode, applied: true}
	}
}

func (m tuiModel) shellHelperCmd() tea.Cmd {
	helper := m.shellHelper
	return func() tea.Msg {
		if helper == nil {
			return shellHelperMsg{unavailable: true}
		}
		info, err := helper()
		if err != nil {
			return shellHelperMsg{err: err.Error()}
		}
		return shellHelperMsg{info: info}
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
		m.state = connectcore.State(msg)
		switch m.state.Status {
		case connectcore.StatusPreparing:
			// Only a fresh ConnectTarget passes through preparing (a failover
			// recovery re-promotes via connecting), so this is where a genuinely
			// new session starts: drop the previous session's activity notice.
			m.activity = connectcore.Notice{}
			m.activityAt = time.Time{}
		case connectcore.StatusConnected:
			// Stamp only a fresh session: an automatic failover re-promotes
			// through connecting → connected without ending the telemetry
			// session, so an existing stamp is kept. A manual switch or a
			// disconnect passes through disconnected, which clears it below.
			if m.connectedAt.IsZero() {
				m.connectedAt = time.Now()
			}
		case connectcore.StatusDisconnected, connectcore.StatusFailed:
			m.connectedAt = time.Time{}
			// Health probes belong to the session that just ended; the last
			// activity notice stays visible so a failure remains explainable.
			m.health = connectcore.Notice{}
		}
		return m, m.pollCmd()

	case engineNoticeMsg:
		notice := connectcore.Notice(msg)
		if notice.Kind == connectcore.NoticeHealthProbe {
			m.health = notice
		} else {
			m.activity = notice
			m.activityAt = time.Now()
		}
		return m, nil

	case shellHelperMsg:
		m.settings.shell = msg.info
		m.settings.shellOK = !msg.unavailable && msg.err == ""
		switch {
		case msg.unavailable:
			m.settings.shellErr = settingsNote{kind: noteShellUnavailable}
		case msg.err != "":
			m.settings.shellErr = settingsNote{kind: noteText, text: msg.err}
		default:
			m.settings.shellErr = settingsNote{}
		}
		return m, nil

	case modeSetMsg:
		if !msg.applied {
			m.settings.note = settingsNote{kind: noteText, text: msg.err}
			return m, nil
		}
		m.settings.mode = msg.mode
		if msg.mode == connectcore.ModeTUN {
			m.settings.note = settingsNote{kind: noteModeTUN}
		} else {
			m.settings.note = settingsNote{kind: noteModeProxy}
		}
		// Switching into proxy mode needs the stable endpoint the launch-time
		// resolution skipped.
		if msg.mode == connectcore.ModeProxy && m.proxyEndpoint == "" && m.proxyErr == "" {
			return m, m.resolveProxyCmd()
		}
		return m, nil

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
		m.view = viewRelays
		return m, nil
	case "2":
		m.view = viewLogs
		return m, nil
	case "3":
		m.view = viewSettings
		return m, nil
	case "0":
		// Cycle the UI language in place — no settings entry, so a user who
		// cannot read the current language never has to navigate one. The
		// footer advertises the key trilingually (languageKeyHelp) for the
		// same reason.
		//
		// A digit, not a letter, because this key must be TYPEABLE in the same
		// situation it must be readable. A Cyrillic (ЙЦУКЕН) or Greek layout
		// carries no Latin letters at all, so a letter binding is unreachable
		// without switching the OS layout first — for exactly the reader who
		// cannot read the UI telling them which key to press. Digits sit on
		// every layout, the same property that makes the 1-4 view keys work,
		// and 0 reads as their neighbour.
		m.lang = (m.lang + 1) % languageCount
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
			// The mode is the engine's to accept: it refuses mid-session, and
			// TUN mode is refused again at connect time without the privileges
			// to create the tunnel device.
			m.settings.note = settingsNote{}
			return m, m.setModeCmd(toggleMode(m.settings.mode))
		}
		if m.settings.cursor == fieldShellHelper {
			// Mirrors the desktop Settings gating: the enable command points a
			// shell at the local endpoint, which only answers while connected.
			if m.settings.mode == connectcore.ModeTUN {
				m.settings.note = settingsNote{kind: noteShellTUN}
				return m, nil
			}
			if m.state.Status != connectcore.StatusConnected {
				m.settings.note = settingsNote{kind: noteShellDisconnected}
				return m, nil
			}
			m.settings.note = settingsNote{}
			return m, m.shellHelperCmd()
		}
		m.settings.editing = true
		m.settings.note = settingsNote{}
		m.settings.input.SetValue(m.settingsFieldValue(m.settings.cursor))
		m.settings.input.CursorEnd()
		return m, m.settings.input.Focus()
	}
	return m, nil
}

func (m tuiModel) handleSettingsEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		// The editor captures printable keys (so q stays typed text), but the
		// quit chord must always work.
		return m, tea.Quit
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
	if field == fieldBroker {
		return m.settings.brokerURL
	}
	return ""
}

func (m *tuiModel) applySettingsField(field settingsFieldID, value string) {
	if field == fieldBroker {
		m.settings.brokerURL = value
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
