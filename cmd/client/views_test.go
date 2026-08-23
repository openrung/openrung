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
		if got := countryFlag(cc); got != want {
			t.Errorf("countryFlag(%q) = %q, want %q", cc, got, want)
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
// connection state, summary or not.
func TestHeaderAndFooterNeverExceedNarrowWidths(t *testing.T) {
	m := newTestModel(&fakeDriver{})
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
				if w := lipgloss.Width(m.headerView()); w > width {
					t.Errorf("width %d lang %d: header is %d cells wide", width, lang, w)
				}
				if w := lipgloss.Width(m.footerView()); w > width {
					t.Errorf("width %d lang %d status %s: footer is %d cells wide", width, lang, status, w)
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
