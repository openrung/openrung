package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/openrung/openrung/connectcore"
)

func TestLanguageKeyCyclesWithoutEnteringSettings(t *testing.T) {
	m := newTestModel(&fakeDriver{})

	if !strings.Contains(m.View(), "1 Relays") {
		t.Fatal("default view is not English")
	}

	m, _ = update(t, m, keyMsg("0"))
	if m.view != viewRelays {
		t.Fatalf("language key changed the view to %d", m.view)
	}
	// Evidence from both a tab and the key help, so this covers more than the
	// header: the status bar's own labels scroll, so they are not dependable
	// here (TestSettingsNotesFollowLanguageCycles covers body text).
	view := m.View()
	if !strings.Contains(view, "1 中继") || !strings.Contains(view, "c 连接") {
		t.Fatalf("first cycle is not Chinese:\n%s", view)
	}

	m, _ = update(t, m, keyMsg("0"))
	view = m.View()
	if !strings.Contains(view, "1 Узлы") || !strings.Contains(view, "c подключить") {
		t.Fatalf("second cycle is not Russian:\n%s", view)
	}

	m, _ = update(t, m, keyMsg("0"))
	if view = m.View(); !strings.Contains(view, "1 Relays") {
		t.Fatalf("third cycle did not wrap back to English:\n%s", view)
	}

	// Keys this control used to live on must be inert, so a stale muscle
	// memory or doc cannot silently half-work.
	for _, stale := range []string{"5", ".", "l"} {
		m, _ = update(t, m, keyMsg(stale))
		if view = m.View(); !strings.Contains(view, "1 Relays") || m.view != viewRelays {
			t.Fatalf("%q still cycles the language or moves the view:\n%s", stale, view)
		}
	}
}

// The language key is a digit, and broker URLs carry digits (ports): an
// editor with focus must swallow it as text rather than cycling the UI out
// from under the value being typed.
func TestLanguageKeyIsTextWhileEditingSettings(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewSettings // cursor starts on the broker field

	m, _ = update(t, m, keyMsg("enter"))
	if !m.settings.editing {
		t.Fatal("enter did not begin editing")
	}
	before, seeded := m.lang, m.settings.input.Value()
	for _, r := range ":8080" {
		m, _ = update(t, m, keyMsg(string(r)))
	}
	if m.lang != before {
		t.Fatalf("typing a digit cycled the language to %d", m.lang)
	}
	// The editor opens pre-filled with the current value, so the keys append.
	if got := m.settings.input.Value(); got != seeded+":8080" {
		t.Fatalf("editor value = %q, want %q with the typed digits retained", got, seeded+":8080")
	}
}

// Settings notices are stored as kinds and worded at draw time, so a note
// set under one language follows a language cycle into the next — stored
// rendered text would leave mixed-language UI for exactly the reader the
// trilingual escape hatch serves.
func TestSettingsNotesFollowLanguageCycles(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.view = viewSettings
	m.lang = langChinese

	m, _ = update(t, m, modeSetMsg{mode: connectcore.ModeTUN, applied: true})
	if view := m.View(); !strings.Contains(view, "TUN 模式：下次连接将接管所有应用") {
		t.Fatalf("Chinese mode note missing:\n%s", view)
	}

	m, _ = update(t, m, keyMsg("0")) // 中文 → русский
	view := m.View()
	if !strings.Contains(view, "режим TUN: следующее подключение захватит все приложения") {
		t.Fatalf("note did not follow the language cycle:\n%s", view)
	}
	if strings.Contains(view, "TUN 模式") {
		t.Fatalf("stale Chinese note survived the cycle:\n%s", view)
	}

	// The shell-unavailable notice takes the same path. Back to proxy mode
	// first: the TUN mode set above masks the Shell proxy row's value.
	m, _ = update(t, m, modeSetMsg{mode: connectcore.ModeProxy, applied: true})
	m, _ = update(t, m, shellHelperMsg{unavailable: true})
	if !strings.Contains(m.View(), "интеграция с shell недоступна") {
		t.Fatalf("Russian shell-unavailable notice missing:\n%s", m.View())
	}
	m, _ = update(t, m, keyMsg("0")) // русский → English
	if !strings.Contains(m.View(), "shell integration is unavailable in this build") {
		t.Fatalf("shell notice did not follow the cycle:\n%s", m.View())
	}
}

// The language switch must be findable in every language, so the RENDERED
// footer carries the trilingual token whenever it fits — and the header, now
// views-only, no longer holds a language slot. Asserting on tr().helpGlobal
// instead would be a tautology: helpGlobal is built by concatenating
// languageKeyHelp at init, so such a check passes however the footer renders.
// Narrow terminals, where the token has to scroll into view, are covered by
// TestFooterMarqueeRevealsLanguageToken.
func TestRenderedFooterShowsTheTrilingualLanguageHelp(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	m.width = 120 // wide enough that no view's help needs the marquee
	for lang := language(0); lang < languageCount; lang++ {
		m.lang = lang
		for v := viewID(0); v < viewCount; v++ {
			m.view = v
			if footer := m.footerView(); !strings.Contains(footer, languageKeyHelp) {
				t.Errorf("lang %d view %d footer lost the language help: %q", lang, v, footer)
			}
		}
		if header := m.headerView(); strings.Contains(header, "язык") {
			t.Fatalf("lang %d header still carries a language slot: %q", lang, header)
		}
	}
}

// Every language must fill every string, and any string carrying fmt verbs
// must carry as many as English does — a missing %s in one language would
// render a bare "%!s(MISSING)" only for those users.
func TestTranslationTablesAreCompleteAndFormatCompatible(t *testing.T) {
	english := reflect.ValueOf(*translations[langEnglish])
	statuses := []connectcore.Status{
		connectcore.StatusDisconnected, connectcore.StatusPreparing,
		connectcore.StatusConnecting, connectcore.StatusConnected,
		connectcore.StatusDisconnecting, connectcore.StatusFailed,
	}

	for lang := language(0); lang < languageCount; lang++ {
		tr := translations[lang]
		if tr == nil {
			t.Fatalf("lang %d has no translation table", lang)
		}
		v := reflect.ValueOf(*tr)
		for i := 0; i < v.NumField(); i++ {
			name := v.Type().Field(i).Name
			switch field := v.Field(i); field.Kind() {
			case reflect.String:
				got := field.String()
				if strings.TrimSpace(got) == "" {
					t.Errorf("lang %d: %s is empty", lang, name)
				}
				if want := strings.Count(english.Field(i).String(), "%"); strings.Count(got, "%") != want {
					t.Errorf("lang %d: %s has %d fmt verbs, English has %d", lang, name, strings.Count(got, "%"), want)
				}
			case reflect.Array:
				for j := 0; j < field.Len(); j++ {
					if strings.TrimSpace(field.Index(j).String()) == "" {
						t.Errorf("lang %d: %s[%d] is empty", lang, name, j)
					}
				}
			case reflect.Map:
				for _, status := range statuses {
					if strings.TrimSpace(tr.statusName(status)) == "" || tr.statusName(status) == string(status) && lang != langEnglish {
						t.Errorf("lang %d: status %q is untranslated", lang, status)
					}
				}
			}
		}
	}
}
