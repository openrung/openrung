package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore"
)

// allStatuses is every connection state the bars must handle.
var allStatuses = []connectcore.Status{
	connectcore.StatusDisconnected, connectcore.StatusPreparing,
	connectcore.StatusConnecting, connectcore.StatusConnected,
	connectcore.StatusDisconnecting, connectcore.StatusFailed,
}

func TestCountryFlag(t *testing.T) {
	cases := map[string]string{
		"jp": "🇯🇵", "JP": "🇯🇵", " us ": "🇺🇸", "KR": "🇰🇷",
		"": "", "j": "", "jpn": "", "1a": "",
	}
	for cc, want := range cases {
		if got := countryFlagFor("darwin", cc); got != want {
			t.Errorf("countryFlagFor(darwin, %q) = %q, want %q", cc, got, want)
		}
	}
	// Windows terminals render the pair as two separate emoji-width glyphs —
	// wider than the counted width — which overflowed the padded footer bar
	// and scrolled the frame on every repaint. No flag may ever render there.
	for _, cc := range []string{"jp", "US", "kr"} {
		if got := countryFlagFor("windows", cc); got != "" {
			t.Errorf("countryFlagFor(windows, %q) = %q, want the empty string", cc, got)
		}
	}
}

// The country rides on the pin, as a code AND a flag — never the flag alone,
// since countryFlag renders nothing on Windows and the pin is now the only
// place a country appears.
func TestStatusPinPairsCountryCodeWithFlag(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.state = connectcore.State{Status: connectcore.StatusConnected}
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}
	pin := m.statusPin(0)
	if !strings.Contains(pin, "JP"+countryFlagFor("darwin", "jp")) {
		t.Fatalf("pin did not pair the country code with its flag:\n%q", pin)
	}
	// The code is what survives where the flag cannot render.
	if !strings.Contains(pin, "JP") {
		t.Fatalf("pin lost the country code:\n%q", pin)
	}
}

func TestRelaysViewShowsLabelOnlyWithFlag(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewRelays
	m.refreshing = false
	m.relays = []connectcore.DirectoryRelay{
		{Relay: brokerapi.RelayDescriptor{
			ID: "r1", Label: "merry-falcon",
			RelayGeoLocation: brokerapi.RelayGeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "jp"},
		}},
		// No label: the geo name is the only identity left, so it stays.
		{Relay: brokerapi.RelayDescriptor{
			ID:               "r2",
			RelayGeoLocation: brokerapi.RelayGeoLocation{City: "Helsinki", Country: "Finland", CountryCode: "fi"},
		}},
	}

	view := m.View()
	if !strings.Contains(view, "merry-falcon") {
		t.Fatalf("labeled relay not listed by its label:\n%s", view)
	}
	if strings.Contains(view, "Tokyo") {
		t.Fatalf("labeled relay still shows its geo name:\n%s", view)
	}
	if !strings.Contains(view, "JP🇯🇵") || !strings.Contains(view, "FI🇫🇮") {
		t.Fatalf("country column missing flags:\n%s", view)
	}
	if !strings.Contains(view, "Helsinki, Finland") {
		t.Fatalf("unlabeled relay lost its geo fallback name:\n%s", view)
	}
}

// Every row the old Status view carried has to survive somewhere: the bar's
// detail track holds them all, and the session duration is additionally pinned
// to the rendered bar's right edge.
func TestStatusDetailCarriesEveryStatusField(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 400 // wide enough that nothing is windowed away
	label := "merry-falcon"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{
		Transport: "wss", FrontID: "front-a",
		Relay: brokerapi.RelayDescriptor{
			Label: "merry-falcon", NodeClass: brokerapi.NodeClassFoundation,
			RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
		},
	}
	m.connectedAt = m.now.Add(-90 * time.Second)
	m.proxyEndpoint = "127.0.0.1:43210"
	m.health = connectcore.Notice{Kind: connectcore.NoticeHealthProbe}
	failure := "tunnel process exited"
	m.state.LastError = &failure

	tr := m.tr()
	detail := m.statusDetail()
	for _, want := range []string{
		"[foundation]", tr.labelTransport, "front-a", tr.labelHealth,
		tr.labelCapture, tr.labelProxy, "127.0.0.1:43210", tr.labelBroker,
		tr.labelTarget, tr.labelError, failure,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("status detail missing %q:\n%s", want, detail)
		}
	}
	// Nothing the bar states another way belongs in the scrolling track: the
	// state is the bar's color, and the relay and its country are pinned. Any
	// of them repeated here reads as a second, different relay.
	for _, gone := range []string{"Status", "Country", "merry-falcon", "JP"} {
		if strings.Contains(detail, gone) {
			t.Errorf("%q is duplicated into the scrolling detail:\n%s", gone, detail)
		}
	}
	for _, want := range []string{"merry-falcon", "JP", "00:01:30"} {
		if bar := m.statusFooterView(); !strings.Contains(bar, want) {
			t.Errorf("status bar pin missing %q:\n%q", want, bar)
		}
	}

	// Disconnected: no duration is pinned, and the key help never carries one.
	m.state.Status = connectcore.StatusDisconnected
	m.connectedAt = time.Time{}
	if bar := m.statusFooterView(); strings.Contains(bar, "00:01:30") {
		t.Errorf("disconnected status bar still pins a duration:\n%q", bar)
	}
	if footer := m.footerView(); strings.Contains(footer, "00:01:30") ||
		strings.Contains(footer, "merry-falcon") {
		t.Errorf("key help carries connection state that belongs on the bar:\n%q", footer)
	}
}

// The connection signal moved to the status bar, so the key help must render
// with no background of its own — the theme paints it like any other line.
func TestKeyHelpFooterIsNotColored(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	m := newTestModel(&fakeDriver{})
	m.width = 100
	for _, status := range allStatuses {
		m.state = connectcore.State{Status: status}
		if footer := m.footerView(); strings.Contains(footer, "\x1b[") {
			t.Errorf("status %s: key help carries styling: %q", status, footer)
		}
		if bar := m.statusFooterView(); !strings.Contains(bar, "\x1b[") {
			t.Errorf("status %s: status bar lost its connection color: %q", status, bar)
		}
	}
}

// Relay identity comes from ONE source: with a descriptor, both the name (down
// to the "relay <id>" fallback) and the country are the descriptor's; the state
// label appears only when there is no descriptor at all, and then without a
// country. A failover updates the state label before the next info poll, so any
// mixing would pair the new relay's name with the old relay's country.
func TestStatusIdentityIsSingleSourced(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 400
	newLabel := "Singapore"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &newLabel}
	m.connectedAt = m.now.Add(-time.Minute)

	// Stale descriptor: nameless, but still carrying the OLD country.
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		ID:               "r9",
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}
	pin := m.statusPin(0)
	if strings.Contains(pin, "Singapore") {
		t.Fatalf("pin paired the state label with the descriptor's flag: %q", pin)
	}
	if !strings.Contains(pin, "relay r9") {
		t.Fatalf("pin did not fall back to the descriptor's own name: %q", pin)
	}
	if !strings.Contains(pin, "JP") {
		t.Fatalf("pin lost the descriptor's country: %q", pin)
	}

	// No descriptor: the state label stands alone, and no country is asserted.
	m.infoOK = false
	pin = m.statusPin(0)
	if !strings.Contains(pin, "Singapore") {
		t.Fatalf("descriptor-less pin lost the state label: %q", pin)
	}
	if strings.Contains(pin, "JP") || strings.Contains(pin, countryFlagFor("darwin", "jp")) {
		t.Fatalf("descriptor-less pin still asserts a country: %q", pin)
	}
}

// The relay name lives only on the pin, so the pin has to survive the
// transitions: "connecting" must still name its destination, while a
// disconnected or failed bar asserts no relay at all.
func TestStatusPinNamesTheRelayThroughTransitions(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 120
	label := "merry-falcon"

	for _, status := range []connectcore.Status{
		connectcore.StatusPreparing, connectcore.StatusConnecting,
		connectcore.StatusConnected, connectcore.StatusDisconnecting,
	} {
		m.state = connectcore.State{Status: status, RelayLabel: &label}
		if pin := m.statusPin(0); !strings.Contains(pin, label) {
			t.Errorf("status %s: pin does not name the relay: %q", status, pin)
		}
	}
	// No duration outside a live session, even with a stamp left over.
	m.connectedAt = m.now.Add(-time.Minute)
	m.state = connectcore.State{Status: connectcore.StatusConnecting, RelayLabel: &label}
	if pin := m.statusPin(0); strings.Contains(pin, "00:01:00") {
		t.Errorf("connecting pin shows a session duration: %q", pin)
	}
	for _, status := range []connectcore.Status{
		connectcore.StatusDisconnected, connectcore.StatusFailed,
	} {
		m.state = connectcore.State{Status: status, RelayLabel: &label}
		if pin := m.statusPin(0); pin != "" {
			t.Errorf("status %s: stale relay lingers on the pin: %q", status, pin)
		}
	}
}

// The pin holds both answers a glance needs — which relay, and for how long —
// single-sourced from the descriptor when there is one. Under width pressure
// the label yields and the duration stays, since "how long" is the field the
// pin exists for.
func TestStatusPinCarriesRelayLabelAndDuration(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 120
	stale := "Singapore"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &stale}
	m.connectedAt = m.now.Add(-90 * time.Second)
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		Label:            "merry-falcon",
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}

	flag := countryFlagFor("darwin", "jp")
	bar := m.statusFooterView()
	for _, want := range []string{"merry-falcon", flag, "00:01:30"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("status bar pin missing %q:\n%q", want, bar)
		}
	}
	// The pin is the bar's right edge: nothing but its trailing space follows.
	if !strings.HasSuffix(strings.TrimRight(bar, " "), "00:01:30") {
		t.Fatalf("duration is not anchored to the right edge:\n%q", bar)
	}
	// Single-sourced: the descriptor won, so the stale state label is absent.
	if strings.Contains(m.statusPin(0), stale) {
		t.Fatalf("pin used the state label over the descriptor: %q", m.statusPin(0))
	}

	// Squeezed: the label gives way, the duration survives.
	narrow := m.statusPin(statusPinMinWidth)
	if narrow != "00:01:30" {
		t.Fatalf("pin at its floor = %q, want the bare duration", narrow)
	}

	// Disconnected: no pin at all.
	m.state.Status = connectcore.StatusDisconnected
	if pin := m.statusPin(0); pin != "" {
		t.Fatalf("disconnected pin = %q, want empty", pin)
	}
}

// However narrow the terminal, and at every marquee phase, the status bar keeps
// the session duration pinned to its right edge: the detail track is what
// yields. That corner glance is the reason the bar exists.
func TestStatusBarKeepsDurationPinnedWhileScrolling(t *testing.T) {
	for _, width := range []int{20, 30, 60, 80} {
		m := newTestModel(&fakeDriver{})
		m.width = width
		label := "merry-falcon"
		m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
		m.startedAt = time.Unix(0, 0)
		m.connectedAt = m.startedAt.Add(-time.Minute)

		steps := footerMarqueePause + lipgloss.Width(m.statusDetail()) +
			lipgloss.Width(footerMarqueeGap) + 2
		for step := 0; step < steps; step++ {
			m.now = m.startedAt.Add(time.Duration(step) * footerMarqueeStep)
			bar := m.statusFooterView()
			if want := formatDuration(m.now.Sub(m.connectedAt)); !strings.Contains(bar, want) {
				t.Fatalf("w=%d step=%d: status bar dropped %q: %q", width, step, want, bar)
			}
			if w := lipgloss.Width(bar); w > width {
				t.Fatalf("w=%d step=%d: status bar is %d cells: %q", width, step, w, bar)
			}
		}
	}
}

// footerHelpWindow must return exactly the width it was asked for at EVERY
// marquee phase. ansi.Cut counts cells but cannot split one, so a boundary
// landing on the double-width CJK in languageKeyHelp used to return a cell
// too many — and disconnected, where no summary absorbs it, the bar shipped
// one cell past the terminal (width 80, Relays, English, step 111).
func TestFooterMarqueeHoldsItsWidthAtEveryPhase(t *testing.T) {
	names := []string{"status", "relays", "logs", "settings"}
	label := "merry-falcon"
	for _, width := range []int{20, 30, 40, 60, 80, 100, 120} {
		for _, connected := range []bool{false, true} {
			for lang := language(0); lang < languageCount; lang++ {
				for v := viewID(0); v < viewCount; v++ {
					m := newTestModel(&fakeDriver{})
					m.width, m.lang, m.view = width, lang, v
					m.startedAt = time.Unix(0, 0)
					if connected {
						m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
						m.connectedAt = m.startedAt.Add(-time.Minute)
					}
					// One full cycle: the pause, the whole track, and the gap.
					steps := footerMarqueePause + lipgloss.Width(m.tr().helpGlobal) +
						lipgloss.Width(m.tr().helpRelays) + lipgloss.Width(footerMarqueeGap) + 2
					for step := 0; step < steps; step++ {
						m.now = m.startedAt.Add(time.Duration(step) * footerMarqueeStep)
						for bar, got := range map[string]string{
							"help":   m.footerView(),
							"status": m.statusFooterView(),
						} {
							if w := lipgloss.Width(got); w > width {
								t.Fatalf("w=%d %s lang=%d conn=%t step=%d: %s bar is %d cells",
									width, names[v], lang, connected, step, bar, w)
							}
						}
					}
				}
			}
		}
	}
}

// Narrow terminals scroll the help, so the complete language token has to come
// into view within one cycle — it is the one control a reader stuck in an
// unreadable language depends on. Below ~30 cells the lane cannot hold the
// token's 16 cells alongside the session duration, which is a known gap.
func TestFooterMarqueeRevealsLanguageToken(t *testing.T) {
	label := "merry-falcon"
	for _, width := range []int{30, 40, 60, 80} {
		for _, connected := range []bool{false, true} {
			for lang := language(0); lang < languageCount; lang++ {
				for v := viewID(0); v < viewCount; v++ {
					m := newTestModel(&fakeDriver{})
					m.width, m.lang, m.view = width, lang, v
					m.startedAt = time.Unix(0, 0)
					if connected {
						m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
						m.connectedAt = m.startedAt.Add(-time.Minute)
					}
					steps := footerMarqueePause + lipgloss.Width(m.tr().helpGlobal) +
						lipgloss.Width(m.tr().helpRelays) + lipgloss.Width(footerMarqueeGap) + 2
					seen := false
					for step := 0; step < steps && !seen; step++ {
						m.now = m.startedAt.Add(time.Duration(step) * footerMarqueeStep)
						seen = strings.Contains(m.footerView(), languageKeyHelp)
					}
					if !seen {
						t.Errorf("w=%d view %d lang %d conn=%t: a full cycle never showed %q",
							width, v, lang, connected, languageKeyHelp)
					}
				}
			}
		}
	}
}

// Header and footer each budget exactly one line, so on a narrow terminal
// they must shed content instead of wrapping — in every language and every
// connection state, summary or not. What they shed follows priority: the
// header keeps the active tab over the title and inactive tabs, and the
// footer keeps the session duration over the label.
func TestHeaderAndFooterNeverExceedNarrowWidths(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewSettings
	label := "merry-falcon"
	for _, width := range []int{20, 40, 60} {
		m.width = width
		for lang := language(0); lang < languageCount; lang++ {
			m.lang = lang
			for _, status := range []connectcore.Status{connectcore.StatusDisconnected, connectcore.StatusConnected} {
				m.state = connectcore.State{Status: status, RelayLabel: &label}
				if status == connectcore.StatusConnected {
					m.connectedAt = m.now.Add(-time.Minute)
				}
				header := m.headerView()
				if w := lipgloss.Width(header); w > width {
					t.Errorf("width %d lang %d: header is %d cells wide", width, lang, w)
				}
				if !strings.Contains(header, m.tr().tabs[viewSettings]) {
					t.Errorf("width %d lang %d: header dropped the active tab: %q", width, lang, header)
				}
				footer := m.footerView()
				if w := lipgloss.Width(footer); w > width {
					t.Errorf("width %d lang %d status %s: footer is %d cells wide", width, lang, status, w)
				}
				bar := m.statusFooterView()
				if w := lipgloss.Width(bar); w > width {
					t.Errorf("width %d lang %d status %s: status bar is %d cells wide", width, lang, status, w)
				}
				if status == connectcore.StatusConnected && !strings.Contains(bar, "00:01:00") {
					t.Errorf("width %d lang %d: status bar lost the session duration: %q", width, lang, bar)
				}
			}
		}
	}
}

// A wide-rune (CJK) relay label must not push its row's columns out of
// alignment: truncation and padding both go by display cells.
func TestRelayRowsAlignWithCJKLabels(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewRelays
	m.refreshing = false
	m.relays = []connectcore.DirectoryRelay{
		{Relay: brokerapi.RelayDescriptor{ID: "r1", Label: strings.Repeat("首尔中继节点", 5),
			RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "kr"}}},
		{Relay: brokerapi.RelayDescriptor{ID: "r2", Label: "plain-ascii",
			RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "us"}}},
	}

	var widths []int
	for _, row := range strings.Split(m.relaysView(), "\n") {
		if strings.Contains(row, "[") { // the badge marks a relay row
			widths = append(widths, lipgloss.Width(row))
		}
	}
	if len(widths) != 2 {
		t.Fatalf("expected 2 relay rows, got %d", len(widths))
	}
	if widths[0] != widths[1] {
		t.Fatalf("CJK row is %d cells, ASCII row is %d — columns misaligned", widths[0], widths[1])
	}
}

// The brand mark sits whole in the bottom-right corner of every non-Logs view,
// its last row on the last body line — directly above the status bar — and
// never pushes a line past the terminal width.
func TestBrandMarkOnNonLogViews(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width, m.height = 100, 30
	for v := viewID(0); v < viewCount; v++ {
		if v == viewLogs {
			continue
		}
		m.view = v
		view := m.View()
		for _, artLine := range openrungArt {
			if !strings.Contains(view, artLine) {
				t.Fatalf("view %d missing brand-mark row %q:\n%s", v, artLine, view)
			}
		}
		lines := strings.Split(view, "\n")
		for i, line := range lines {
			if w := lipgloss.Width(line); w > m.width {
				t.Errorf("view %d line %d is %d cells wide, max %d", v, i, w, m.width)
			}
		}
		last := lines[len(lines)-1-footerHeight] // the last body line
		if !strings.HasSuffix(last, openrungArt[len(openrungArt)-1]) {
			t.Fatalf("view %d: brand mark not on the last body line:\n%q", v, last)
		}
		if w := lipgloss.Width(last); w != m.width-brandMargin {
			t.Errorf("view %d: brand mark ends at cell %d, want %d (right-aligned)", v, w, m.width-brandMargin)
		}
	}
}

// Logs keep the whole viewport for diagnostics: in particular, the newest
// rows at the bottom must not lose their right-hand side under decoration.
func TestBrandMarkIsDisabledInLogs(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width, m.height = 80, 24
	m.view = viewLogs
	m.resizeLogView()
	marker := "newest-log-details-" + strings.Repeat("x", 50)
	lines := make([]string, m.bodyHeight())
	for i := range lines {
		lines[i] = "older"
	}
	lines[len(lines)-1] = marker
	m.setLogLines(lines)

	view := m.View()
	if strings.Contains(view, openrungArt[0]) {
		t.Fatalf("Logs view still carries the brand mark:\n%s", view)
	}
	if !strings.Contains(view, marker) {
		t.Fatalf("Logs view cut the newest row:\n%s", view)
	}
}

// A terminal too narrow or too short for the mark drops it entirely — a torn
// or crowding mark is worse than none.
func TestBrandMarkHiddenOnSmallTerminals(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = lipgloss.Width(openrungArt[0]) + 3 // below the mark plus its clearance
	if view := m.View(); strings.Contains(view, openrungArt[0]) {
		t.Fatalf("narrow terminal still draws the brand mark:\n%s", view)
	}
	m.width, m.height = 100, len(openrungArt)+headerHeight+footerHeight+1 // one body row above the mark
	if view := m.View(); strings.Contains(view, openrungArt[0]) {
		t.Fatalf("short terminal still draws the brand mark:\n%s", view)
	}
}

// Outside Logs, the mark wins the corner over body content, and the cut under
// it is ANSI-aware so styled body lines are never severed mid-escape-sequence.
func TestBrandMarkWinsTheCornerOverContent(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width, m.height = 60, 24
	wide := errorStyle.Render(strings.Repeat("x", m.width)) // full-width AND styled
	body := m.overlayBrandMark(fitLines(strings.Repeat(wide+"\n", 19)+wide, m.bodyHeight()))
	lines := strings.Split(body, "\n")
	for _, line := range lines[len(lines)-len(openrungArt):] {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("overlaid line is %d cells wide, max %d: %q", w, m.width, line)
		}
	}
	if !strings.Contains(body, openrungArt[0]) {
		t.Fatalf("full-width content displaced the brand mark:\n%s", body)
	}
}

// paintFrame lays the green-on-black base under every cell: lines pad to the
// terminal width so the background reaches the right edge, and an inner
// style's reset hands control back to the base, not the terminal defaults.
func TestPaintFrameRepaintsDefaults(t *testing.T) {
	frame := "plain\n\x1b[1mbold\x1b[0m tail"
	lines := strings.Split(paintFrame(frame, 12), "\n")
	if len(lines) != 2 {
		t.Fatalf("paintFrame changed the line count: %q", lines)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, themeSeq) || !strings.HasSuffix(line, resetSeq) {
			t.Errorf("line %d not wrapped in the theme base: %q", i, line)
		}
		if w := lipgloss.Width(line); w != 12 {
			t.Errorf("line %d painted to %d cells, want the full 12", i, w)
		}
	}
	if !strings.Contains(lines[1], resetSeq+themeSeq) {
		t.Error("inner reset not re-opened with the theme base")
	}
}

// Settings holds exactly broker, mode, and the shell helper: the target relay
// is pinned from the Relays view (or CLI flags), never typed into Settings —
// and the tab bar keys, obvious enough to go unadvertised, stay out of the
// footer help.
func TestSettingsOffersNoTargetFields(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewSettings
	m.settings.target = connectcore.RelayTarget{RelayID: "r1", Label: "x", Country: "kr"}
	view := m.settingsView()
	for _, gone := range []string{"Target relay id", "Target label", "Target country"} {
		if strings.Contains(view, gone) {
			t.Fatalf("settings still offers %q:\n%s", gone, view)
		}
	}
	if strings.Contains(m.footerView(), "1-4") {
		t.Fatalf("footer still advertises the view keys: %q", m.footerView())
	}
}

func TestFooterStyleCoversEveryStatus(t *testing.T) {
	for _, status := range allStatuses {
		if _, ok := footerStyles[status]; !ok {
			t.Errorf("footerStyles missing %q", status)
		}
	}
}
