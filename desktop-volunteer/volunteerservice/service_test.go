package volunteerservice

import (
	"strings"
	"testing"

	"openrung/desktop-volunteer/persist"
)

// newTestService builds a service with storage rooted in a temp dir and the
// engine wired, without going through Wails Startup.
func newTestService(t *testing.T) *Service {
	t.Helper()
	s := New()
	s.store = persist.NewInDir(t.TempDir())
	s.settings = s.store.LoadSettings()
	s.buildEngine()
	return s
}

func TestStartRequiresConsent(t *testing.T) {
	s := newTestService(t)
	s.XrayFound = true
	if err := s.Start(); err == nil || !strings.Contains(err.Error(), "consent") {
		t.Fatalf("Start without consent = %v, want consent error", err)
	}
	if err := s.AcceptConsent(); err != nil {
		t.Fatalf("AcceptConsent: %v", err)
	}
	if !s.GetState().ConsentAccepted {
		t.Fatal("consent not reflected in state")
	}
}

func TestStartRequiresXray(t *testing.T) {
	s := newTestService(t)
	_ = s.AcceptConsent()
	s.XrayFound = false
	if err := s.Start(); err == nil || !strings.Contains(err.Error(), "xray") {
		t.Fatalf("Start without xray = %v, want xray error", err)
	}
}

func TestSaveSettingsValidatesAndNormalizes(t *testing.T) {
	s := newTestService(t)

	if _, err := s.SaveSettings(Settings{Label: "ok", MaxSessions: 0, MaxMbps: 20, ListenPort: 8443}); err == nil {
		t.Fatal("expected max sessions validation error")
	}
	if _, err := s.SaveSettings(Settings{Label: "bad name!", MaxSessions: 8, MaxMbps: 20, ListenPort: 8443}); err == nil {
		t.Fatal("expected label validation error")
	}

	out, err := s.SaveSettings(Settings{Label: "  My.Relay-1  ", MaxSessions: 4, MaxMbps: 50, ListenPort: 9443, BrokerURL: "", HubAddress: " hub.example:9443 "})
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if out.Label != "My.Relay-1" {
		t.Fatalf("label = %q", out.Label)
	}
	if out.BrokerURL != DefaultBrokerURL {
		t.Fatalf("broker URL = %q, want default", out.BrokerURL)
	}
	if out.HubAddress != "hub.example:9443" {
		t.Fatalf("hub = %q", out.HubAddress)
	}

	// Settings survive a reload through the same store.
	reloaded := s.store.LoadSettings()
	if reloaded.MaxMbps != 50 || reloaded.ListenPort != 9443 {
		t.Fatalf("persisted settings = %+v", reloaded)
	}
}

func TestSaveSettingsEmptyLabelGeneratesOne(t *testing.T) {
	s := newTestService(t)
	out, err := s.SaveSettings(Settings{Label: "", MaxSessions: 8, MaxMbps: 20, ListenPort: 8443})
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if out.Label == "" {
		t.Fatal("expected a generated label")
	}
}

func TestRegenerateLabelChangesAndPersists(t *testing.T) {
	s := newTestService(t)
	first, err := s.RegenerateLabel()
	if err != nil {
		t.Fatalf("RegenerateLabel: %v", err)
	}
	if first == "" {
		t.Fatal("empty label")
	}
	if got := s.store.LoadSettings().Label; got != first {
		t.Fatalf("persisted label = %q, want %q", got, first)
	}
}

func TestEngineConfigDerivesModeFromHub(t *testing.T) {
	s := newTestService(t)
	s.mu.Lock()
	cfg := s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "direct" {
		t.Fatalf("mode without hub = %q, want direct", cfg.Mode)
	}

	if _, err := s.SaveSettings(Settings{MaxSessions: 8, MaxMbps: 20, ListenPort: 8443, HubAddress: "hub.example:9443"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	s.mu.Lock()
	cfg = s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "auto" {
		t.Fatalf("mode with hub = %q, want auto", cfg.Mode)
	}
	if !cfg.PunchCapable {
		t.Fatal("punch should be offered")
	}
}

func TestStateDefaults(t *testing.T) {
	s := newTestService(t)
	state := s.GetState()
	if state.Phase != "idle" {
		t.Fatalf("phase = %q, want idle", state.Phase)
	}
	if state.Running {
		t.Fatal("running should be false")
	}
	if state.Settings.ListenPort != defaultListenPort {
		t.Fatalf("default listen port = %d", state.Settings.ListenPort)
	}
	if state.Settings.BrokerURL != DefaultBrokerURL {
		t.Fatalf("default broker = %q", state.Settings.BrokerURL)
	}
}
