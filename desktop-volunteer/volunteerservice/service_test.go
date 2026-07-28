package volunteerservice

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"openrung/desktop-volunteer/directsetup"
	"openrung/desktop-volunteer/persist"
	"openrung/internal/relayruntime"
)

type fakeDirectSetup struct {
	status      directsetup.Status
	statusCalls int
	enableCalls int
	removeCalls int
	enableErr   error
}

type blockingDirectSetup struct {
	staleStatus   directsetup.Status
	removedStatus directsetup.Status
	statusStarted chan struct{}
	releaseStatus chan struct{}
	removeCalled  chan struct{}
}

func (f *blockingDirectSetup) Status(context.Context) directsetup.Status {
	close(f.statusStarted)
	<-f.releaseStatus
	return f.staleStatus
}

func (f *blockingDirectSetup) Enable(context.Context) (directsetup.Status, error) {
	return f.staleStatus, nil
}

func (f *blockingDirectSetup) Remove(context.Context) (directsetup.Status, error) {
	close(f.removeCalled)
	return f.removedStatus, nil
}

func (f *fakeDirectSetup) Status(context.Context) directsetup.Status {
	f.statusCalls++
	return f.status
}

func (f *fakeDirectSetup) Enable(context.Context) (directsetup.Status, error) {
	f.enableCalls++
	return f.status, f.enableErr
}

func (f *fakeDirectSetup) Remove(context.Context) (directsetup.Status, error) {
	f.removeCalls++
	return f.status, nil
}

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

func TestDefaultCapacitySettings(t *testing.T) {
	s := newTestService(t)
	settings := s.GetState().Settings
	if settings.MaxSessions != relayruntime.DefaultMaxSessions || settings.MaxMbps != relayruntime.DefaultMaxMbps {
		t.Fatalf("default capacity = %d sessions / %d Mbps, want %d / %d",
			settings.MaxSessions, settings.MaxMbps,
			relayruntime.DefaultMaxSessions, relayruntime.DefaultMaxMbps)
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
	if want := []int{directsetup.DirectPort, defaultListenPort}; !slices.Equal(cfg.AutomaticPortCandidates, want) {
		t.Fatalf("default automatic candidates = %v, want %v", cfg.AutomaticPortCandidates, want)
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
	if len(cfg.AutomaticPortCandidates) != 0 {
		t.Fatalf("direct-only automatic candidates = %v, want none", cfg.AutomaticPortCandidates)
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

func TestPersisted8443MigratesTo443FirstWithoutRewritingSetting(t *testing.T) {
	store := persist.NewInDir(t.TempDir())
	persisted := persist.Settings{
		ListenPort:     8443,
		ConnectionMode: ModeAutomatic,
	}
	if err := store.SaveSettings(persisted); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	s := New(testComponentVersion)
	s.store = store
	s.settings = store.LoadSettings()
	cfg := s.engineConfigLocked()

	if got := s.GetSettings().ListenPort; got != 8443 {
		t.Fatalf("persisted alternate was rewritten to %d, want 8443", got)
	}
	if want := []int{443, 8443}; !slices.Equal(cfg.AutomaticPortCandidates, want) {
		t.Fatalf("automatic candidates = %v, want %v", cfg.AutomaticPortCandidates, want)
	}

	s.settings.ListenPort = 443
	cfg = s.engineConfigLocked()
	if want := []int{443}; !slices.Equal(cfg.AutomaticPortCandidates, want) {
		t.Fatalf("deduplicated automatic candidates = %v, want %v", cfg.AutomaticPortCandidates, want)
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

func TestPublicEndpointFormatting(t *testing.T) {
	if got := formatPublicEndpoint("203.0.113.8", 443); got != "203.0.113.8:443" {
		t.Fatalf("IPv4 endpoint = %q", got)
	}
	if got := formatPublicEndpoint("2001:db8::8", 8443); got != "[2001:db8::8]:8443" {
		t.Fatalf("IPv6 endpoint = %q", got)
	}
}

func TestDirectSetupStatusIsCachedInEmittedState(t *testing.T) {
	fake := &fakeDirectSetup{status: directsetup.Status{
		Platform:  "test",
		State:     directsetup.StateNeedsSetup,
		Reason:    directsetup.ReasonFirewallRuleMissing,
		CanEnable: true,
		Port:      directsetup.DirectPort,
		Message:   "setup needed",
	}}
	s := NewWithDirectSetup(testComponentVersion, fake)
	s.store = persist.NewInDir(t.TempDir())
	s.settings = s.store.LoadSettings()
	s.buildEngine()

	got := s.GetDirectSetupStatus()
	if got != fake.status {
		t.Fatalf("GetDirectSetupStatus = %+v, want %+v", got, fake.status)
	}
	if state := s.GetState(); state.DirectSetup != fake.status {
		t.Fatalf("State.DirectSetup = %+v, want cached %+v", state.DirectSetup, fake.status)
	}

	// Snapshot/event refreshes use only the cache and must not run an external
	// platform inspection every second.
	_ = s.GetState()
	_ = s.GetState()
	if fake.statusCalls != 1 {
		t.Fatalf("cached state caused %d platform inspections, want 1", fake.statusCalls)
	}
}

func TestDirectSetupEnableFailureDoesNotBlockVolunteerService(t *testing.T) {
	fake := &fakeDirectSetup{
		status: directsetup.Status{
			Platform:  "test",
			State:     directsetup.StateNeedsSetup,
			CanEnable: true,
			Port:      directsetup.DirectPort,
			Message:   "alternate ports remain available",
		},
		enableErr: errors.New("authorization declined"),
	}
	s := NewWithDirectSetup(testComponentVersion, fake)
	s.store = persist.NewInDir(t.TempDir())
	s.settings = s.store.LoadSettings()
	s.buildEngine()

	status, err := s.EnableDirectConnections()
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("EnableDirectConnections error = %v", err)
	}
	if status != fake.status || fake.enableCalls != 1 {
		t.Fatalf("status/calls = %+v / %d", status, fake.enableCalls)
	}

	// Setup failure changes neither consent nor the relay engine; volunteering
	// remains independently usable with automatic alternate/RelayHub fallback.
	if err := s.AcceptConsent(); err != nil {
		t.Fatalf("AcceptConsent after setup decline: %v", err)
	}
	if !s.GetState().ConsentAccepted {
		t.Fatal("setup decline blocked normal volunteer state")
	}
}

func TestCapabilityRemovalRequiresRestartBeforeRelayCanStart(t *testing.T) {
	fake := &fakeDirectSetup{status: directsetup.Status{
		Platform: "linux",
		State:    directsetup.StateUnavailable,
		Reason:   directsetup.ReasonRemovalRestartRequired,
		Port:     directsetup.DirectPort,
		Message:  "quit and reopen to complete capability removal",
	}}
	s := NewWithDirectSetup(testComponentVersion, fake)
	s.store = persist.NewInDir(t.TempDir())
	s.settings = s.store.LoadSettings()
	s.settings.ConsentAccepted = true
	s.directSetupStatus = fake.status
	s.XrayFound = true
	s.buildEngine()

	err := s.Start()
	if err == nil || !strings.Contains(err.Error(), "quit and reopen") {
		t.Fatalf("Start after capability removal = %v, want restart gate", err)
	}
	if s.Running() {
		t.Fatal("engine started before capability removal restart")
	}
}

func TestSetupRefreshAndRemovalAreSerializedWithoutStaleOverwrite(t *testing.T) {
	stale := directsetup.Status{
		Platform:  "linux",
		State:     directsetup.StateReady,
		Reason:    directsetup.ReasonReady,
		CanRemove: true,
		Port:      directsetup.DirectPort,
		Message:   "stale ready status",
	}
	removed := directsetup.Status{
		Platform: "linux",
		State:    directsetup.StateUnavailable,
		Reason:   directsetup.ReasonRemovalRestartRequired,
		Port:     directsetup.DirectPort,
		Message:  "quit and reopen",
	}
	fake := &blockingDirectSetup{
		staleStatus:   stale,
		removedStatus: removed,
		statusStarted: make(chan struct{}),
		releaseStatus: make(chan struct{}),
		removeCalled:  make(chan struct{}),
	}
	s := NewWithDirectSetup(testComponentVersion, fake)

	refreshDone := make(chan struct{})
	go func() {
		_ = s.GetDirectSetupStatus()
		close(refreshDone)
	}()
	select {
	case <-fake.statusStarted:
	case <-time.After(time.Second):
		t.Fatal("status refresh did not start")
	}

	removeDone := make(chan error, 1)
	go func() {
		_, err := s.RemoveDirectConnections()
		removeDone <- err
	}()
	select {
	case <-fake.removeCalled:
		t.Fatal("removal raced an in-flight status refresh")
	case <-time.After(50 * time.Millisecond):
	}

	close(fake.releaseStatus)
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("status refresh did not finish")
	}
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("RemoveDirectConnections: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serialized removal did not finish")
	}
	if got := s.GetState().DirectSetup; got != removed {
		t.Fatalf("cached setup status = %+v, want final removal status %+v", got, removed)
	}
}
