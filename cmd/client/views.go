package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"openrung/internal/connectcore"
	"openrung/internal/relay"
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

	foundationBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("[foundation]")
	volunteerBadge  = lipgloss.NewStyle().Faint(true).Render("[volunteer]")

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
	names := []string{"1 Status", "2 Relays", "3 Logs", "4 Settings"}
	tabs := make([]string, 0, len(names)+1)
	tabs = append(tabs, titleStyle.Render(" OpenRung "))
	for i, name := range names {
		style := tabStyle
		if viewID(i) == m.view {
			style = tabActiveStyle
		}
		tabs = append(tabs, style.Render(name))
	}
	return strings.Join(tabs, " ")
}

func (m tuiModel) footerView() string {
	help := "c connect · d disconnect · r refresh · 1-4/tab views · q quit"
	switch m.view {
	case viewRelays:
		help = "↑/↓ select · enter connect to selection · x clear target · " + help
	case viewLogs:
		help = "↑/↓/pgup/pgdn scroll · " + help
	case viewSettings:
		if m.settings.editing {
			help = "enter apply · esc cancel"
		} else {
			help = "↑/↓ field · enter edit · " + help
		}
	}
	return helpStyle.Render(help)
}

// ---- Status ----

func (m tuiModel) statusView() string {
	status := m.state.Status
	rows := []string{
		row("Status", statusStyles[status].Render(string(status))),
	}

	relayLine := "—"
	if m.state.RelayLabel != nil {
		relayLine = *m.state.RelayLabel
	}
	if m.infoOK {
		relayLine += "  " + nodeClassBadge(m.info.Relay.NodeClass)
	}
	rows = append(rows, row("Relay", relayLine))

	country, transport := "—", "—"
	if m.infoOK {
		if cc := strings.TrimSpace(m.info.Relay.CountryCode); cc != "" {
			country = strings.ToUpper(cc)
		}
		transport = transportLabel(m.info)
	}
	rows = append(rows, row("Country", country), row("Transport", transport))

	session := "—"
	if status == connectcore.StatusConnected && !m.connectedAt.IsZero() {
		session = formatDuration(m.now.Sub(m.connectedAt))
	}
	rows = append(rows, row("Session", session))

	if status == connectcore.StatusConnected {
		rows = append(rows, row("Health", healthLabel(m.health)))
	}
	if m.activity.Kind != "" {
		stamp := m.activityAt.Format("15:04:05")
		rows = append(rows, row("Activity", noteStyle.Render("["+stamp+"] "+noticeLine(m.activity))))
	}

	if m.settings.mode == connectcore.ModeTUN {
		// No local endpoint exists in TUN mode; the tunnel device carries every
		// application, so there is nothing for the user to configure.
		rows = append(rows, row("Capture", "TUN — whole device"))
	} else {
		proxy := m.proxyEndpoint
		switch {
		case m.proxyErr != "":
			proxy = errorStyle.Render(m.proxyErr)
		case proxy == "":
			proxy = "resolving…"
		}
		rows = append(rows, row("Capture", "proxy — applications configured for the endpoint below"), row("Proxy", proxy))
		if m.proxyWarn != "" {
			rows = append(rows, row("", noteStyle.Render(m.proxyWarn)))
		}
	}

	rows = append(rows,
		row("Broker", displayBroker(m.settings.brokerURL)),
		row("Target", describeTarget(m.settings.target)),
	)

	if m.state.LastError != nil {
		rows = append(rows, "", row("Error", errorStyle.Render(*m.state.LastError)))
	}
	return strings.Join(rows, "\n")
}

func row(label, value string) string {
	return labelStyle.Render(label) + " " + value
}

func displayBroker(brokerURL string) string {
	if strings.TrimSpace(brokerURL) == "" {
		return "default fronts"
	}
	return brokerURL
}

func transportLabel(info connectcore.ConnectionInfo) string {
	switch info.Transport {
	case relay.TransportDirect:
		return "direct"
	case "punch":
		return "punched (direct NAT path)"
	case "wss":
		return "WSS front " + info.FrontID
	default:
		return info.Transport
	}
}

func nodeClassBadge(nodeClass string) string {
	if nodeClass == relay.NodeClassFoundation {
		return foundationBadge
	}
	// A missing class is the volunteer class, per the descriptor contract.
	return volunteerBadge
}

// healthLabel renders the latest mid-session probe sweep. The engine only
// probes while a candidate is promoted, so before the first sweep of a session
// there is nothing to report yet.
func healthLabel(health connectcore.Notice) string {
	if health.Kind == "" {
		return helpStyle.Render("probing every 30s…")
	}
	if health.Failures == 0 {
		return "ok"
	}
	label := fmt.Sprintf("%d/%d probes failed", health.Failures, health.Threshold)
	if health.Reason != "" {
		label += " — " + health.Reason
	}
	return errorStyle.Render(label)
}

// noticeLine formats a typed engine notice for the Activity row.
func noticeLine(n connectcore.Notice) string {
	switch n.Kind {
	case connectcore.NoticeFailoverStarted:
		return fmt.Sprintf("failover: relay %s lost (%s); re-laddering", n.FromRelayID, n.Reason)
	case connectcore.NoticeFailoverCompleted:
		line := fmt.Sprintf("failover: relay %s → %s (%s)", n.FromRelayID, n.RelayID, n.Reason)
		if n.FrontID != "" {
			line += " via WSS front " + n.FrontID
		}
		return line
	case connectcore.NoticeWSSFallback:
		return fmt.Sprintf("WSS fallback: relay %s via front %s (direct path: %s)", n.RelayID, n.FrontID, n.Reason)
	case connectcore.NoticeWSSTicketRetry:
		return fmt.Sprintf("WSS tickets rate-limited; retrying front %s in %s", n.FrontID, n.Wait)
	case connectcore.NoticePunchOutcome:
		return fmt.Sprintf("punch %s: %s", n.RelayID, n.Reason)
	}
	return n.Reason
}

func describeTarget(target connectcore.RelayTarget) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(target.RelayID) != "" {
		parts = append(parts, "relay "+target.RelayID)
	}
	if strings.TrimSpace(target.Label) != "" {
		parts = append(parts, "label "+target.Label)
	}
	if strings.TrimSpace(target.Country) != "" {
		parts = append(parts, "country "+strings.ToUpper(strings.TrimSpace(target.Country)))
	}
	if len(parts) == 0 {
		return "automatic (ranked)"
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
	var rows []string
	// The engine-persisted recents (internal/clientstate), newest first — the
	// same row the desktop main screen shows.
	if line := recentsLine(m.state.Recents); line != "" {
		rows = append(rows, helpStyle.Render("recents ")+truncate(line, max(1, m.width-8)))
	}
	switch {
	case m.refreshing:
		rows = append(rows, helpStyle.Render("refreshing relay directory…"))
	case m.relayErr != "":
		rows = append(rows, errorStyle.Render("directory: "+m.relayErr))
	case len(m.relays) == 0:
		rows = append(rows, helpStyle.Render("no relays yet — press r to refresh"))
	}
	if len(m.relays) == 0 {
		return strings.Join(rows, "\n")
	}

	rows = append(rows, helpStyle.Render(fmt.Sprintf("   %-28s %-8s %-9s %s", "RELAY", "COUNTRY", "LATENCY", "CLASS")))

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
			nodeClassBadge(entry.Relay.NodeClass),
		)
		if entry.Relay.ID == m.settings.target.RelayID && m.settings.target.RelayID != "" {
			line += " " + noteStyle.Render("← target")
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
func relayDisplayName(r relay.Descriptor) string {
	geo := geoDisplayLabel(r)
	if label := strings.TrimSpace(r.Label); label != "" && label != geo {
		return geo + " (" + label + ")"
	}
	return geo
}

// geoDisplayLabel mirrors the engine's geoLabel presentation rule: city and
// country when known, never a raw IP.
func geoDisplayLabel(r relay.Descriptor) string {
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
		return helpStyle.Render("no log output yet")
	}
	return m.logView.View()
}

// ---- Settings ----

func (m tuiModel) settingsView() string {
	fields := []struct {
		id    settingsFieldID
		name  string
		value string
	}{
		{fieldBroker, "Broker URL", displayBroker(m.settings.brokerURL)},
		{fieldMode, "Mode", modeLabel(m.settings.mode)},
		{fieldRelayID, "Target relay id", orUnset(m.settings.target.RelayID)},
		{fieldRelayLabel, "Target label", orUnset(m.settings.target.Label)},
		{fieldCountry, "Target country", orUnset(m.settings.target.Country)},
		{fieldShellHelper, "Shell proxy", m.shellHelperValue()},
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
			rows = append(rows, "  "+labelStyle.Width(18).Render("Enable in a shell")+" "+m.settings.shell.EnableCommand)
		} else {
			rows = append(rows, "  "+labelStyle.Width(18).Render("Enable in a shell")+" "+helpStyle.Render("available while connected"))
		}
		rows = append(rows,
			"  "+labelStyle.Width(18).Render("Restore that shell")+" "+m.settings.shell.DisableCommand,
			"  "+helpStyle.Render("run the restore command after disconnect, failure, quit, or crash"),
		)
	}
	if m.settings.note != "" {
		rows = append(rows, "", noteStyle.Render(m.settings.note))
	}
	return strings.Join(rows, "\n")
}

// shellHelperValue is the Shell proxy row's summary cell.
func (m tuiModel) shellHelperValue() string {
	switch {
	case m.settings.mode == connectcore.ModeTUN:
		return helpStyle.Render("not needed in TUN mode")
	case m.settings.shellErr != "":
		return errorStyle.Render(m.settings.shellErr)
	case m.settings.shellOK:
		return "commands below"
	case m.state.Status == connectcore.StatusConnected:
		return helpStyle.Render("press enter to show the shell commands")
	default:
		return helpStyle.Render("available while connected")
	}
}

// modeLabel is the Settings Mode row: what the mode does and what it costs.
func modeLabel(mode connectcore.Mode) string {
	if mode == connectcore.ModeTUN {
		return "TUN — whole device (needs sudo)"
	}
	return "proxy — local mixed HTTP/SOCKS inbound (no privileges)"
}

// modeNote explains what a just-accepted mode changes, since the engine only
// applies it on the next connect.
func modeNote(mode connectcore.Mode) string {
	if mode == connectcore.ModeTUN {
		return "TUN mode: the next connect captures every application — the client must be running under sudo"
	}
	return "proxy mode: the next connect serves a local mixed proxy and points the system proxy at it"
}

func toggleMode(mode connectcore.Mode) connectcore.Mode {
	if mode == connectcore.ModeTUN {
		return connectcore.ModeProxy
	}
	return connectcore.ModeTUN
}

func orUnset(value string) string {
	if strings.TrimSpace(value) == "" {
		return helpStyle.Render("(unset)")
	}
	return value
}
