package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore"
)

// Rendering only: everything here formats state the engine already delivered.

var (
	titleStyle     = lipgloss.NewStyle().Bold(true)
	tabStyle       = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	tabActiveStyle = lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1)
	labelStyle     = lipgloss.NewStyle().Faint(true).Width(12)
	helpStyle      = lipgloss.NewStyle().Faint(true)
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	noteStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cursorStyle    = lipgloss.NewStyle().Bold(true)
	brandStyle     = lipgloss.NewStyle().Bold(true)

	foundationBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	volunteerBadgeStyle  = lipgloss.NewStyle().Faint(true)

	statusStyles = map[connectcore.Status]lipgloss.Style{
		connectcore.StatusDisconnected:  lipgloss.NewStyle().Faint(true),
		connectcore.StatusPreparing:     lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		connectcore.StatusConnecting:    lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		connectcore.StatusConnected:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true),
		connectcore.StatusDisconnecting: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		connectcore.StatusFailed:        lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
	}

	// The footer is a solid tmux-style status bar spanning the full terminal
	// width — the always-visible connection signal: red while disconnected or
	// failed, yellow through every transition, green while connected.
	footerStyles = map[connectcore.Status]lipgloss.Style{
		connectcore.StatusDisconnected:  lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("1")),
		connectcore.StatusFailed:        lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("1")),
		connectcore.StatusPreparing:     lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")),
		connectcore.StatusConnecting:    lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")),
		connectcore.StatusDisconnecting: lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")),
		connectcore.StatusConnected:     lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("2")),
	}
)

const (
	headerHeight = 2 // tab bar + separator blank line
	footerHeight = 2 // separator blank line + key help
)

func (m tuiModel) bodyHeight() int {
	return m.height - headerHeight - footerHeight
}

func (m tuiModel) View() string {
	if m.width == 0 {
		return "" // no size yet; first WindowSizeMsg is about to arrive
	}
	var body string
	switch m.view {
	case viewStatus:
		body = m.statusView()
	case viewRelays:
		body = m.relaysView()
	case viewLogs:
		body = m.logsView()
	case viewSettings:
		body = m.settingsView()
	}
	body = m.overlayBrandMark(fitLines(body, m.bodyHeight()))
	frame := m.headerView() + "\n\n" + body + "\n\n" + m.footerView()
	// Ascii means no color at all (a dumb terminal, NO_COLOR, or a test),
	// where the theme's escape sequences would render as garbage.
	if lipgloss.ColorProfile() != termenv.Ascii {
		frame = paintFrame(frame, m.width)
	}
	return frame
}

// openrungArt is the corner brand mark, drawn at the bottom right of every
// view just above the status bar. Every row is the same display width.
var openrungArt = []string{
	"  ___  ___  ___ _  _ ___  _   _ _  _  ___ ",
	" / _ \\| _ \\| __| \\| | _ \\| | | | \\| |/ __|",
	"| (_) |  _/| _|| .` |   /| |_| | .` | (_ |",
	" \\___/|_|  |___|_|\\_|_|_\\ \\___/|_|\\_|\\___|",
}

// brandMargin keeps the mark off the terminal's right edge.
const brandMargin = 1

// overlayBrandMark draws openrungArt over the body's bottom-right corner —
// the same corner on every view. The mark wins over whatever the corner held:
// a per-line collision check would tear the mark apart (logs) or blink it in
// and out as lists grow (relays), and the corner rows are the least
// load-bearing cells on screen. It renders whole or not at all — only when
// the terminal is wide enough for the mark plus a gap to content, and tall
// enough that a couple of content rows stay clear above it.
func (m tuiModel) overlayBrandMark(body string) string {
	artW := lipgloss.Width(openrungArt[0])
	if m.width < artW+2+2*brandMargin || m.bodyHeight() < len(openrungArt)+2 {
		return body
	}
	lines := strings.Split(body, "\n")
	start := len(lines) - len(openrungArt)
	col := m.width - artW - brandMargin
	for i, artLine := range openrungArt {
		// ANSI-aware cut: body lines carry styled segments whose escape
		// sequences a byte or rune slice would sever mid-sequence.
		line := ansi.Truncate(lines[start+i], col-1, "")
		pad := col - lipgloss.Width(line)
		lines[start+i] = line + strings.Repeat(" ", max(0, pad)) + brandStyle.Render(artLine)
	}
	return strings.Join(lines, "\n")
}

// The old-terminal theme: green text on a black background.
const (
	themeSeq = "\x1b[32;40m" // green foreground, black background
	resetSeq = "\x1b[0m"
)

// paintFrame lays the theme base under the composed frame. Padding every line
// to the full terminal width carries the black background to the right edge,
// and every reset an inner style emits is immediately re-opened with the base
// sequence, so text after a styled segment falls back to green-on-black
// instead of the terminal's own defaults. Styles that pick their colors — the
// status bar, errors, notes, badges — are untouched: only default-colored
// text turns green.
func paintFrame(frame string, width int) string {
	lines := strings.Split(frame, "\n")
	for i, line := range lines {
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		lines[i] = themeSeq + strings.ReplaceAll(line, resetSeq, resetSeq+themeSeq) + resetSeq
	}
	return strings.Join(lines, "\n")
}

// fitLines pads or truncates body to exactly n lines so header and footer stay
// pinned regardless of view content.
func fitLines(body string, n int) string {
	if n < 1 {
		return ""
	}
	lines := strings.Split(body, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) headerView() string {
	tr := m.tr()
	tabs := make([]string, 0, len(tr.tabs)+1)
	tabs = append(tabs, titleStyle.Render(" OpenRung v"+strings.TrimSpace(baseVersion)+" "))
	for i, name := range tr.tabs {
		style := tabStyle
		if viewID(i) == m.view {
			style = tabActiveStyle
		}
		tabs = append(tabs, style.Render(name))
	}
	// The language switch lives on the l key and is advertised in the footer
	// (languageKeyHelp), not here — the tab bar holds only views.

	// headerHeight budgets exactly one line, so a narrow terminal sheds whole
	// tabs (the keys still work). Shedding goes by importance, not position:
	// the active tab outranks the title, which outranks inactive view tabs.
	// Whatever fits renders in bar order.
	type slot struct {
		order int
		label string
	}
	priority := make([]slot, 0, len(tabs))
	priority = append(priority,
		slot{1 + int(m.view), tabs[1+int(m.view)]},
		slot{0, tabs[0]},
	)
	for i := viewID(0); i < viewCount; i++ {
		if i != m.view {
			priority = append(priority, slot{1 + int(i), tabs[1+int(i)]})
		}
	}

	picked := make([]string, len(tabs))
	used := -1 // the first rendered tab pays no separator
	for _, s := range priority {
		w := lipgloss.Width(s.label) + 1
		if m.width > 0 && used+w > m.width {
			continue
		}
		used += w
		picked[s.order] = s.label
	}
	parts := make([]string, 0, len(picked))
	for _, label := range picked {
		if label != "" {
			parts = append(parts, label)
		}
	}
	return strings.Join(parts, " ")
}

func (m tuiModel) footerView() string {
	tr := m.tr()
	help := tr.helpGlobal
	switch m.view {
	case viewRelays:
		help = tr.helpRelays + help
	case viewLogs:
		help = tr.helpLogs + help
	case viewSettings:
		if m.settings.editing {
			help = tr.helpSettingsEdit
		} else {
			help = tr.helpSettings + help
		}
	}

	style, ok := footerStyles[m.state.Status]
	if !ok {
		style = helpStyle
	}
	// The bar is padded to the exact terminal width so the background paints
	// edge to edge, with the connection summary right-aligned in the corner.
	// The help always yields to the width — a wrapped footer breaks the
	// fixed-height layout — and to the summary, since that corner glance is
	// what the bar is for.
	summaryBudget := 0 // ≤0 = unlimited
	if m.width > 0 {
		summaryBudget = m.width - 2 // the bar's leading and trailing space
	}
	right := m.footerConnectionSummary(summaryBudget)
	if right != "" {
		right += " "
	}
	if m.width > 0 {
		budget := m.width - lipgloss.Width(right) - 1 // leading space
		if right != "" {
			budget-- // one-cell gap between help and summary
		}
		if lipgloss.Width(help) > budget {
			help = truncateWidth(help, max(0, budget))
		}
	}
	left := " " + help
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	return style.Render(left + strings.Repeat(" ", max(0, pad)) + right)
}

// footerConnectionSummary is the bottom-right corner while connected: the
// relay label, its country flag, and the running session duration, fitted to
// budget cells (≤0 = unlimited). The duration anchors the right edge, so when
// space runs out the label gives way first — a glance at the corner should
// always answer "how long", even on a terminal too narrow for "to what".
func (m tuiModel) footerConnectionSummary(budget int) string {
	if m.state.Status != connectcore.StatusConnected {
		return ""
	}
	// Every identity field comes from ONE source. With a descriptor, that is
	// the descriptor — name (down to its "relay <id>" fallback) AND flag: on
	// a failover the state label updates before the next info poll lands, and
	// mixing the two would pair a fresh label with the old relay's flag,
	// asserting the wrong exit country. Without a descriptor there is no
	// flag, only the state label.
	label := ""
	if m.infoOK {
		label = strings.TrimSpace(relayListName(m.info.Relay))
		if flag := countryFlag(m.info.Relay.CountryCode); flag != "" {
			if label != "" {
				label += " "
			}
			label += flag
		}
	} else if m.state.RelayLabel != nil {
		label = strings.TrimSpace(*m.state.RelayLabel)
	}
	duration := ""
	if !m.connectedAt.IsZero() {
		duration = formatDuration(m.now.Sub(m.connectedAt))
	}

	parts := make([]string, 0, 2)
	if label != "" {
		parts = append(parts, label)
	}
	if duration != "" {
		parts = append(parts, duration)
	}
	summary := strings.Join(parts, " ")
	if budget <= 0 || lipgloss.Width(summary) <= budget {
		return summary
	}
	if duration == "" {
		return truncateWidth(label, budget)
	}
	label = truncateWidth(label, max(0, budget-lipgloss.Width(duration)-1))
	if label == "" {
		return truncateWidth(duration, budget)
	}
	return label + " " + duration
}

// countryFlag maps a 2-letter ISO country code to its regional-indicator
// emoji ("jp" → 🇯🇵), or "" for anything else — and "" for everything on
// Windows. Windows terminals do not combine the two regional indicators into
// one flag glyph: they draw two separate emoji-width characters, typically
// four cells where the width libraries count two. Any line padded against the
// counted width — the footer status bar — then physically overflows the
// terminal and wraps, scrolling the alt screen one row on every repaint and
// leaving a stack of stale bars (the Windows smoke-test finding; it appeared
// only while connected because that is the only footer that carries a flag).
// The glyphs would not have read as a flag there anyway, and every site that
// renders one pairs it with the country code, which remains.
func countryFlag(cc string) string {
	return countryFlagFor(runtime.GOOS, cc)
}

// countryFlagFor is countryFlag with the platform explicit, so both branches
// are testable from any platform.
func countryFlagFor(goos, cc string) string {
	if goos == "windows" {
		return ""
	}
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
		return ""
	}
	return string(0x1F1E6+rune(cc[0])-'A') + string(0x1F1E6+rune(cc[1])-'A')
}

// ---- Status ----

func (m tuiModel) statusView() string {
	tr := m.tr()
	status := m.state.Status
	rows := []string{
		row(tr.labelStatus, statusStyles[status].Render(tr.statusName(status))),
	}

	relayLine := "—"
	if m.state.RelayLabel != nil {
		relayLine = *m.state.RelayLabel
	}
	if m.infoOK {
		relayLine += "  " + nodeClassBadge(tr, m.info.Relay.NodeClass)
	}
	rows = append(rows, row(tr.labelRelay, relayLine))

	country, transport := "—", "—"
	if m.infoOK {
		if cc := strings.TrimSpace(m.info.Relay.CountryCode); cc != "" {
			country = strings.ToUpper(cc) + countryFlag(cc)
		}
		transport = transportLabel(tr, m.info)
	}
	rows = append(rows, row(tr.labelCountry, country), row(tr.labelTransport, transport))

	session := "—"
	if status == connectcore.StatusConnected && !m.connectedAt.IsZero() {
		session = formatDuration(m.now.Sub(m.connectedAt))
	}
	rows = append(rows, row(tr.labelSession, session))

	if status == connectcore.StatusConnected {
		rows = append(rows, row(tr.labelHealth, healthLabel(tr, m.health)))
	}
	if m.activity.Kind != "" {
		stamp := m.activityAt.Format("15:04:05")
		rows = append(rows, row(tr.labelActivity, noteStyle.Render("["+stamp+"] "+noticeLine(tr, m.activity))))
	}

	if m.settings.mode == connectcore.ModeTUN {
		// No local endpoint exists in TUN mode; the tunnel device carries every
		// application, so there is nothing for the user to configure.
		rows = append(rows, row(tr.labelCapture, tr.captureTUN))
	} else {
		proxy := m.proxyEndpoint
		switch {
		case m.proxyErr != "":
			proxy = errorStyle.Render(m.proxyErr)
		case proxy == "":
			proxy = tr.proxyResolving
		}
		rows = append(rows, row(tr.labelCapture, tr.captureProxy), row(tr.labelProxy, proxy))
		if m.proxyWarn != "" {
			rows = append(rows, row("", noteStyle.Render(m.proxyWarn)))
		}
	}

	rows = append(rows,
		row(tr.labelBroker, displayBroker(tr, m.settings.brokerURL)),
		row(tr.labelTarget, describeTarget(tr, m.settings.target)),
	)

	if m.state.LastError != nil {
		rows = append(rows, "", row(tr.labelError, errorStyle.Render(*m.state.LastError)))
	}
	return strings.Join(rows, "\n")
}

func row(label, value string) string {
	return labelStyle.Render(label) + " " + value
}

func displayBroker(tr *translation, brokerURL string) string {
	if strings.TrimSpace(brokerURL) == "" {
		return tr.defaultFronts
	}
	return brokerURL
}

func transportLabel(tr *translation, info connectcore.ConnectionInfo) string {
	switch info.Transport {
	case brokerapi.TransportDirect:
		return tr.transportDirect
	case "punch":
		return tr.transportPunched
	case "wss":
		return fmt.Sprintf(tr.transportWSS, info.FrontID)
	default:
		return info.Transport
	}
}

func nodeClassBadge(tr *translation, nodeClass string) string {
	// A missing or unrecognized class is the volunteer class, per the
	// descriptor contract; EffectiveNodeClass is that rule.
	if brokerapi.EffectiveNodeClass(nodeClass) == brokerapi.NodeClassFoundation {
		return foundationBadgeStyle.Render(tr.badgeFoundation)
	}
	return volunteerBadgeStyle.Render(tr.badgeVolunteer)
}

// healthLabel renders the latest mid-session probe sweep. The engine only
// probes while a candidate is promoted, so before the first sweep of a session
// there is nothing to report yet.
func healthLabel(tr *translation, health connectcore.Notice) string {
	if health.Kind == "" {
		return helpStyle.Render(tr.healthProbing)
	}
	if health.Failures == 0 {
		return tr.healthOK
	}
	label := fmt.Sprintf(tr.healthFailed, health.Failures, health.Threshold)
	if health.Reason != "" {
		label += " — " + health.Reason
	}
	return errorStyle.Render(label)
}

// noticeLine formats a typed engine notice for the Activity row. The reasons
// interpolated into each line come from the engine and stay English.
func noticeLine(tr *translation, n connectcore.Notice) string {
	switch n.Kind {
	case connectcore.NoticeFailoverStarted:
		return fmt.Sprintf(tr.noticeFailoverStarted, n.FromRelayID, n.Reason)
	case connectcore.NoticeFailoverCompleted:
		line := fmt.Sprintf(tr.noticeFailoverCompleted, n.FromRelayID, n.RelayID, n.Reason)
		if n.FrontID != "" {
			line += fmt.Sprintf(tr.noticeViaFront, n.FrontID)
		}
		return line
	case connectcore.NoticeWSSFallback:
		return fmt.Sprintf(tr.noticeWSSFallback, n.RelayID, n.FrontID, n.Reason)
	case connectcore.NoticeWSSTicketRetry:
		return fmt.Sprintf(tr.noticeTicketRetry, n.FrontID, n.Wait)
	case connectcore.NoticePunchOutcome:
		return fmt.Sprintf(tr.noticePunch, n.RelayID, n.Reason)
	}
	return n.Reason
}

func describeTarget(tr *translation, target connectcore.RelayTarget) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(target.RelayID) != "" {
		parts = append(parts, fmt.Sprintf(tr.targetRelay, target.RelayID))
	}
	if strings.TrimSpace(target.Label) != "" {
		parts = append(parts, fmt.Sprintf(tr.targetLabel, target.Label))
	}
	if strings.TrimSpace(target.Country) != "" {
		parts = append(parts, fmt.Sprintf(tr.targetCountry, strings.ToUpper(strings.TrimSpace(target.Country))))
	}
	if len(parts) == 0 {
		return tr.targetAutomatic
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// ---- Relays ----

func (m tuiModel) relaysView() string {
	tr := m.tr()
	var rows []string
	switch {
	case m.refreshing:
		rows = append(rows, helpStyle.Render(tr.refreshingDirectory))
	case m.relayErr != "":
		rows = append(rows, errorStyle.Render(tr.directoryErrPrefix+m.relayErr))
	case len(m.relays) == 0:
		rows = append(rows, helpStyle.Render(tr.noRelaysYet))
	}
	if len(m.relays) == 0 {
		return strings.Join(rows, "\n")
	}

	// padCell, not %-8s: the localized headers must pad by display width to
	// stay aligned with the ASCII-formatted rows below.
	rows = append(rows, helpStyle.Render("   "+padCell(tr.colRelay, 28)+" "+padCell(tr.colCountry, 8)+" "+padCell(tr.colLatency, 9)+" "+tr.colClass))

	// Window the list to the rows the body has left after any notice lines and
	// the column header, following the cursor: fitLines hard-truncates the body,
	// so without this the selection could sit on a relay the screen never shows.
	visible := m.bodyHeight() - len(rows)
	if visible < 1 {
		visible = 1
	}
	start := 0
	if m.relayCursor >= visible {
		start = m.relayCursor - visible + 1
	}
	end := start + visible
	if end > len(m.relays) {
		end = len(m.relays)
	}
	for i := start; i < end; i++ {
		entry := m.relays[i]
		cursor := "  "
		if i == m.relayCursor {
			cursor = cursorStyle.Render("▸ ")
		}
		cc := strings.ToUpper(strings.TrimSpace(entry.Relay.CountryCode))
		if cc != "" {
			cc += countryFlag(cc)
		}
		// padCell and truncateWidth throughout: the flag emoji (and any CJK in
		// a label) would break byte- or rune-count alignment.
		line := cursor + " " + padCell(truncateWidth(relayListName(entry.Relay), 28), 28) +
			" " + padCell(cc, 8) +
			" " + padCell(latencyLabel(entry.ProbeMS), 9) +
			" " + nodeClassBadge(tr, entry.Relay.NodeClass)
		if entry.Relay.ID == m.settings.target.RelayID && m.settings.target.RelayID != "" {
			line += " " + noteStyle.Render(tr.targetMarker)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// relayListName is the list name for a relay: its friendly label alone (the
// COUNTRY column already carries the geography), falling back to the geo
// label only when a relay has no label.
func relayListName(r brokerapi.RelayDescriptor) string {
	if label := strings.TrimSpace(r.Label); label != "" {
		return label
	}
	return geoDisplayLabel(r)
}

// geoDisplayLabel mirrors the engine's geoLabel presentation rule: city and
// country when known, never a raw IP.
func geoDisplayLabel(r brokerapi.RelayDescriptor) string {
	city := strings.TrimSpace(r.City)
	country := strings.TrimSpace(r.Country)
	switch {
	case city != "" && country != "":
		return city + ", " + country
	case country != "":
		return country
	case strings.TrimSpace(r.Label) != "":
		return strings.TrimSpace(r.Label)
	default:
		return "relay " + r.ID
	}
}

func latencyLabel(probeMS *int64) string {
	if probeMS == nil {
		return "—"
	}
	return fmt.Sprintf("%d ms", *probeMS)
}

// truncateWidth trims s to at most w display cells (a rune-count cut would
// under-trim CJK and emoji, which occupy two cells per rune).
func truncateWidth(s string, w int) string {
	if w < 1 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > w {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// ---- Logs ----

func (m tuiModel) logsView() string {
	if !m.logReady {
		return helpStyle.Render(m.tr().noLogOutput)
	}
	return m.logView.View()
}

// ---- Settings ----

func (m tuiModel) settingsView() string {
	tr := m.tr()
	fields := []struct {
		id    settingsFieldID
		name  string
		value string
	}{
		{fieldBroker, tr.fieldNames[fieldBroker], displayBroker(tr, m.settings.brokerURL)},
		{fieldMode, tr.fieldNames[fieldMode], modeLabel(tr, m.settings.mode)},
		{fieldShellHelper, tr.fieldNames[fieldShellHelper], m.shellHelperValue()},
	}

	rows := make([]string, 0, len(fields)+2)
	for _, field := range fields {
		cursor := "  "
		if field.id == m.settings.cursor {
			cursor = cursorStyle.Render("▸ ")
		}
		value := field.value
		if m.settings.editing && field.id == m.settings.cursor {
			value = m.settings.input.View()
		}
		rows = append(rows, cursor+labelStyle.Width(18).Render(field.name)+" "+value)
	}
	if m.settings.shellOK {
		// Copyable POSIX shell commands, exactly what desktop's Settings offers.
		// The enable line points a shell at the local endpoint, which only
		// answers while connected, so it is shown only then (desktop disables
		// its copy button the same way); the restore line stays visible — its
		// whole purpose is cleaning up a shell after the tunnel is gone.
		rows = append(rows, "")
		if m.state.Status == connectcore.StatusConnected {
			rows = append(rows, "  "+labelStyle.Width(18).Render(tr.enableInShell)+" "+m.settings.shell.EnableCommand)
		} else {
			rows = append(rows, "  "+labelStyle.Width(18).Render(tr.enableInShell)+" "+helpStyle.Render(tr.availableWhileConnected))
		}
		rows = append(rows,
			"  "+labelStyle.Width(18).Render(tr.restoreShell)+" "+m.settings.shell.DisableCommand,
			"  "+helpStyle.Render(tr.restoreAdvice),
		)
	}
	if m.settings.note.kind != noteNone {
		rows = append(rows, "", noteStyle.Render(renderNote(tr, m.settings.note)))
	}
	return strings.Join(rows, "\n")
}

// shellHelperValue is the Shell proxy row's summary cell.
func (m tuiModel) shellHelperValue() string {
	tr := m.tr()
	switch {
	case m.settings.mode == connectcore.ModeTUN:
		return helpStyle.Render(tr.notNeededTUN)
	case m.settings.shellErr.kind != noteNone:
		return errorStyle.Render(renderNote(tr, m.settings.shellErr))
	case m.settings.shellOK:
		return tr.commandsBelow
	case m.state.Status == connectcore.StatusConnected:
		return helpStyle.Render(tr.pressEnterShell)
	default:
		return helpStyle.Render(tr.availableWhileConnected)
	}
}

// modeLabel is the Settings Mode row: what the mode does and what it costs.
// What TUN costs is platform-specific (sudo on Unix, an elevated terminal on
// Windows), so that wording comes from the same file as the check that
// enforces it — and stays English in every language.
func modeLabel(tr *translation, mode connectcore.Mode) string {
	if mode == connectcore.ModeTUN {
		return fmt.Sprintf(tr.modeTUN, tunModeSummary)
	}
	return tr.modeProxy
}

// renderNote resolves a stored settings notice through the ACTIVE language:
// the kind is stored, the words are chosen at draw time, so a note set while
// the UI spoke Chinese follows an l-key cycle into Russian.
func renderNote(tr *translation, n settingsNote) string {
	switch n.kind {
	case noteText:
		return n.text
	case noteModeTUN:
		// The engine only applies a mode on the next connect, hence advice
		// rather than a state change; the TUN cost is the platform const.
		return fmt.Sprintf(tr.modeNoteTUN, tunModeAdvice)
	case noteModeProxy:
		return tr.modeNoteProxy
	case noteShellTUN:
		return tr.noteShellTUN
	case noteShellDisconnected:
		return tr.noteShellDisconnected
	case noteShellUnavailable:
		return tr.shellUnavailable
	}
	return ""
}

func toggleMode(mode connectcore.Mode) connectcore.Mode {
	if mode == connectcore.ModeTUN {
		return connectcore.ModeProxy
	}
	return connectcore.ModeTUN
}
