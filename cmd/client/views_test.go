package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore"
)

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

func TestStatusViewShowsCountryFlag(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}
	if view := m.statusView(); !strings.Contains(view, "JP🇯🇵") {
		t.Fatalf("status view missing the country flag:\n%s", view)
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

func TestFooterShowsConnectionSummaryWhenConnected(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 120 // wide enough for the full help and the summary together
	label := "merry-falcon"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		Label:            "merry-falcon",
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}
	m.connectedAt = time.Now().Add(-90 * time.Second)
	m.now = time.Now()

	footer := m.footerView()
	for _, want := range []string{"merry-falcon 🇯🇵", "00:01:30", "q"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("connected footer missing %q:\n%q", want, footer)
		}
	}

	m.state.Status = connectcore.StatusDisconnected
	if footer := m.footerView(); strings.Contains(footer, "merry-falcon") || strings.Contains(footer, "00:01:30") {
		t.Fatalf("disconnected footer still shows the session summary:\n%q", footer)
	}
}

// The footer's relay identity comes from ONE source: with a descriptor,
// every field — name (down to the "relay <id>" fallback) and flag — is the
// descriptor's; the state label appears only with no descriptor at all. A
// failover updates the state label before the next info poll, so any mixing
// pairs the new relay's name with the old relay's flag.
func TestFooterIdentityIsSingleSourced(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 120
	newLabel := "Singapore"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &newLabel}
	m.connectedAt = time.Now().Add(-time.Minute)
	m.now = time.Now()

	// Stale descriptor: nameless, but still carrying the OLD country.
	m.infoOK = true
	m.info = connectcore.ConnectionInfo{Relay: brokerapi.RelayDescriptor{
		ID:               "r9",
		RelayGeoLocation: brokerapi.RelayGeoLocation{CountryCode: "jp"},
	}}
	footer := m.footerView()
	if strings.Contains(footer, "Singapore") {
		t.Fatalf("footer paired the state label with the descriptor's flag: %q", footer)
	}
	if !strings.Contains(footer, "relay r9 🇯🇵") {
		t.Fatalf("footer did not fall back to the descriptor's own identity: %q", footer)
	}

	// No descriptor: the state label stands alone, flagless.
	m.infoOK = false
	footer = m.footerView()
	if !strings.Contains(footer, "Singapore") || strings.Contains(footer, "🇯🇵") {
		t.Fatalf("descriptor-less footer should be the bare state label: %q", footer)
	}
}

// The summary must survive a terminal too narrow for both it and the help
// line — the help is what yields.
func TestFooterSummaryWinsOnNarrowTerminals(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 30
	label := "merry-falcon"
	m.state = connectcore.State{Status: connectcore.StatusConnected, RelayLabel: &label}
	m.connectedAt = time.Now().Add(-time.Minute)
	m.now = time.Now()

	footer := m.footerView()
	if !strings.Contains(footer, "merry-falcon") || !strings.Contains(footer, "00:01:00") {
		t.Fatalf("narrow footer dropped the connection summary:\n%q", footer)
	}
}

// Header and footer each budget exactly one line, so on a narrow terminal
// they must shed content instead of wrapping — in every language and every
// connection state, summary or not. What they shed follows priority: the
// header keeps the active tab and the language control over the title and
// inactive tabs, and the footer keeps the session duration over the label.
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
				if width >= 40 && !strings.Contains(header, languageTabLabel) {
					t.Errorf("width %d lang %d: header dropped the language control: %q", width, lang, header)
				}
				footer := m.footerView()
				if w := lipgloss.Width(footer); w > width {
					t.Errorf("width %d lang %d status %s: footer is %d cells wide", width, lang, status, w)
				}
				if status == connectcore.StatusConnected && !strings.Contains(footer, "00:01:00") {
					t.Errorf("width %d lang %d: footer lost the session duration: %q", width, lang, footer)
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

func TestFooterStyleCoversEveryStatus(t *testing.T) {
	for status := range statusStyles {
		if _, ok := footerStyles[status]; !ok {
			t.Errorf("footerStyles missing %q", status)
		}
	}
}
