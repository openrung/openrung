package volunteerservice

import (
	"strings"
	"testing"

	"openrung/desktop-volunteer/persist"
)

const testComponentVersion = "9.8.7"

// newTestService builds a service with storage rooted in a temp dir and the
// engine wired, without going through Wails Startup.
func newTestService(t *testing.T) *Service {
	t.Helper()
	s := New(testComponentVersion)
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
	// Out of the box a hub is configured (DefaultHubAddress), so the app runs in
	// auto mode — probe first, direct when reachable, tunnel through the hub when
	// not — and pins the built-in hub's certificate.
	s.mu.Lock()
	cfg := s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "auto" {
		t.Fatalf("default mode = %q, want auto", cfg.Mode)
	}
	if cfg.HubAddr != DefaultHubAddress {
		t.Fatalf("default hub = %q, want %q", cfg.HubAddr, DefaultHubAddress)
	}
	if cfg.HubCertFingerprint != DefaultHubCertFingerprint {
		t.Fatalf("default hub should carry the pinned fingerprint, got %q", cfg.HubCertFingerprint)
	}
	if !cfg.PunchCapable {
		t.Fatal("punch should be offered")
	}
	if cfg.Version != "desktop-volunteer/"+testComponentVersion {
		t.Fatalf("relay version = %q, want desktop-volunteer/%s", cfg.Version, testComponentVersion)
	}

	// A user-supplied hub is still auto mode, but the built-in pin must NOT be
	// applied to a different hub (its cert would never match).
	if _, err := s.SaveSettings(Settings{MaxSessions: 8, MaxMbps: 20, ListenPort: 8443, HubAddress: "hub.example:9443"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	s.mu.Lock()
	cfg = s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "auto" {
		t.Fatalf("mode with custom hub = %q, want auto", cfg.Mode)
	}
	if cfg.HubCertFingerprint != "" {
		t.Fatalf("custom hub must not inherit the built-in pin, got %q", cfg.HubCertFingerprint)
	}
}

func TestDirectOnlyModeNeverUsesHub(t *testing.T) {
	s := newTestService(t)
	// Direct-only must force engine direct mode even though the built-in hub is
	// configured, so a public-IP volunteer runs independently of the hub.
	if _, err := s.SaveSettings(Settings{MaxSessions: 8, MaxMbps: 20, ListenPort: 8443, ConnectionMode: ModeDirectXe}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	s.mu.Lock()
	cfg := s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "direct" {
		t.Fatalf("direct-only mode = %q, want direct", cfg.Mode)
	}

	// An unrecognized connection mode normalizes back to automatic (→ auto with
	// the default hub).
	if _, err := s.SaveSettings(Settings{MaxSessions: 8, MaxMbps: 20, ListenPort: 8443, ConnectionMode: "bogus"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := s.GetSettings().ConnectionMode; got != ModeAutomatic {
		t.Fatalf("unknown mode normalized to %q, want automatic", got)
	}
	s.mu.Lock()
	cfg = s.engineConfigLocked()
	s.mu.Unlock()
	if cfg.Mode != "auto" {
		t.Fatalf("automatic mode = %q, want auto", cfg.Mode)
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
