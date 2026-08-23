package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/openrung/openrung/connectcore"
)

func TestLanguageKeyCyclesWithoutEnteringSettings(t *testing.T) {
	m := newTestModel(&fakeDriver{})

	if !strings.Contains(m.View(), "2 Relays") {
		t.Fatal("default view is not English")
	}

	m, _ = update(t, m, keyMsg("5"))
	if m.view != viewStatus {
		t.Fatalf("language key changed the view to %d", m.view)
	}
	view := m.View()
	if !strings.Contains(view, "2 中继") || !strings.Contains(view, "状态") {
		t.Fatalf("first cycle is not Chinese:\n%s", view)
	}

	m, _ = update(t, m, keyMsg("5"))
	view = m.View()
	if !strings.Contains(view, "2 Узлы") || !strings.Contains(view, "Статус") {
		t.Fatalf("second cycle is not Russian:\n%s", view)
	}

	m, _ = update(t, m, keyMsg("5"))
	if view = m.View(); !strings.Contains(view, "2 Relays") {
		t.Fatalf("third cycle did not wrap back to English:\n%s", view)
	}
}

// Settings notices are stored as kinds and worded at draw time, so a note
// set under one language follows a 5-key cycle into the next — stored
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

	m, _ = update(t, m, keyMsg("5")) // 中文 → русский
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
	m, _ = update(t, m, keyMsg("5")) // русский → English
	if !strings.Contains(m.View(), "shell integration is unavailable in this build") {
		t.Fatalf("shell notice did not follow the cycle:\n%s", m.View())
	}
}

// The language slot must be readable in every language, so it is the same
// trilingual label in all of them.
func TestHeaderAlwaysShowsTheTrilingualLanguageLabel(t *testing.T) {
	m := newTestModel(&fakeDriver{})
	for lang := language(0); lang < languageCount; lang++ {
		m.lang = lang
		if header := m.headerView(); !strings.Contains(header, languageTabLabel) {
			t.Fatalf("lang %d header lost the language label: %q", lang, header)
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
