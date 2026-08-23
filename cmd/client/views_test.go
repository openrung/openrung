package main

import (
	"strings"
	"testing"
	"time"

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

func TestFooterStyleCoversEveryStatus(t *testing.T) {
	for status := range statusStyles {
		if _, ok := footerStyles[status]; !ok {
			t.Errorf("footerStyles missing %q", status)
		}
	}
}
