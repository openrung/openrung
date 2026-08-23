package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

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
	body = fitLines(body, m.bodyHeight())
	return m.headerView() + "\n\n" + body + "\n\n" + m.footerView()
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
	tabs := make([]string, 0, len(tr.tabs)+2)
	tabs = append(tabs, titleStyle.Render(" OpenRung "))
	for i, name := range tr.tabs {
		style := tabStyle
		if viewID(i) == m.view {
			style = tabActiveStyle
		}
		tabs = append(tabs, style.Render(name))
	}
	// Not a view: 5 cycles the language in place, and the label stays
	// trilingual so it is readable whatever language is active.
	tabs = append(tabs, tabStyle.Render(languageTabLabel))
	return strings.Join(tabs, " ")
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
	return helpStyle.Render(help)
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
			country = strings.ToUpper(cc)
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
	// The engine-persisted recents (internal/clientstate), newest first — the
	// same row the desktop main screen shows.
	if line := recentsLine(m.state.Recents); line != "" {
		prefix := helpStyle.Render(tr.recentsLabel)
		rows = append(rows, prefix+truncate(line, max(1, m.width-lipgloss.Width(prefix))))
	}
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
		line := fmt.Sprintf("%s %-28s %-8s %-9s %s",
			cursor,
			truncate(relayDisplayName(entry.Relay), 28),
			strings.ToUpper(strings.TrimSpace(entry.Relay.CountryCode)),
			latencyLabel(entry.ProbeMS),
			nodeClassBadge(tr, entry.Relay.NodeClass),
		)
		if entry.Relay.ID == m.settings.target.RelayID && m.settings.target.RelayID != "" {
			line += " " + noteStyle.Render(tr.targetMarker)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// recentsLine renders the recents newest-first on one line, or "" when none
// are stored.
func recentsLine(recents []connectcore.RecentNode) string {
	if len(recents) == 0 {
		return ""
	}
	parts := make([]string, 0, len(recents))
	for _, r := range recents {
		label := strings.TrimSpace(r.Label)
		if label == "" {
			label = r.CountryCode
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " · ")
}

// relayDisplayName is the list name for a relay: its geo label, with the
// friendly label alongside when both exist.
func relayDisplayName(r brokerapi.RelayDescriptor) string {
	geo := geoDisplayLabel(r)
	if label := strings.TrimSpace(r.Label); label != "" && label != geo {
		return geo + " (" + label + ")"
	}
	return geo
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

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
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
		{fieldRelayID, tr.fieldNames[fieldRelayID], orUnset(tr, m.settings.target.RelayID)},
		{fieldRelayLabel, tr.fieldNames[fieldRelayLabel], orUnset(tr, m.settings.target.Label)},
		{fieldCountry, tr.fieldNames[fieldCountry], orUnset(tr, m.settings.target.Country)},
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
	if m.settings.note != "" {
		rows = append(rows, "", noteStyle.Render(m.settings.note))
	}
	return strings.Join(rows, "\n")
}

// shellHelperValue is the Shell proxy row's summary cell.
func (m tuiModel) shellHelperValue() string {
	tr := m.tr()
	switch {
	case m.settings.mode == connectcore.ModeTUN:
		return helpStyle.Render(tr.notNeededTUN)
	case m.settings.shellErr != "":
		return errorStyle.Render(m.settings.shellErr)
	case m.settings.shellOK:
		return tr.commandsBelow
	case m.state.Status == connectcore.StatusConnected:
		return helpStyle.Render(tr.pressEnterShell)
	default:
		return helpStyle.Render(tr.availableWhileConnected)
	}
}

// modeLabel is the Settings Mode row: what the mode does and what it costs.
// What TUN costs is platform-specific (sudo on Unix, unsupported on Windows),
// so that wording comes from the same file as the check that enforces it —
// and stays English in every language.
func modeLabel(tr *translation, mode connectcore.Mode) string {
	if mode == connectcore.ModeTUN {
		return fmt.Sprintf(tr.modeTUN, tunModeSummary)
	}
	return tr.modeProxy
}

// modeNote explains what a just-accepted mode changes, since the engine only
// applies it on the next connect.
func modeNote(tr *translation, mode connectcore.Mode) string {
	if mode == connectcore.ModeTUN {
		return fmt.Sprintf(tr.modeNoteTUN, tunModeAdvice)
	}
	return tr.modeNoteProxy
}

func toggleMode(mode connectcore.Mode) connectcore.Mode {
	if mode == connectcore.ModeTUN {
		return connectcore.ModeProxy
	}
	return connectcore.ModeTUN
}

func orUnset(tr *translation, value string) string {
	if strings.TrimSpace(value) == "" {
		return helpStyle.Render(tr.unset)
	}
	return value
}
