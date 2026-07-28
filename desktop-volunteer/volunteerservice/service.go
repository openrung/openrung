// Package volunteerservice exposes the embedded volunteer relay engine to the
// webview. It mirrors the desktop client's vpnservice bridge pattern: one
// bound struct, an Emitter assigned during app startup, every exported
// no-context method callable from the frontend, and state pushed through a
// single coalesced event.
package volunteerservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrung/desktop-volunteer/directsetup"
	"openrung/desktop-volunteer/persist"
	"openrung/internal/relay"
	"openrung/internal/relayruntime"
	"openrung/internal/relayruntime/engine"
)

// DefaultBrokerURL matches the desktop client's primary broker endpoint.
// Volunteer registration is a write path served by the broker origin behind
// this hostname; the CDN discovery fronts the client races are read-only and
// deliberately not used here.
const DefaultBrokerURL = "https://broker.openrung.org/"

// DefaultHubAddress is the relay hub for NAT'd volunteers. A non-empty value
// puts every install in auto mode (probe → direct or tunnel), so IPv4/CGNAT
// homes — which cannot expose an inbound port — reach the network through the
// hub instead of being stuck in public-IPv6-only direct mode.
//
// This is the production relay hub (ap-northeast-2). It is baked in for now so
// the app works out of the box; a later change will serve it (and additional
// hubs) over the broker's signed relay-list channel so the address can rotate
// without a new release.
const DefaultHubAddress = "43.201.124.63:9443"

// DefaultHubCertFingerprint pins DefaultHubAddress's self-signed TLS leaf
// certificate (SHA-256). Relay hubs run on bare IPs and cannot obtain a CA
// certificate, so the app pins the exact certificate instead of trusting a CA
// (MITM-proof without a CA; see engine.hubTLSConfig). Empty disables pinning.
const DefaultHubCertFingerprint = "70c3a26b9ac7315d1975f417eb9eabbecc98ec0e2d5baadb6c224e87fd99c8b5"

const (
	defaultListenPort         = 8443
	logRingCapacity           = 200
	directSetupInspectTimeout = 45 * time.Second
)

// Connection modes exposed to the user.
const (
	ModeAutomatic = "automatic" // probe, then serve directly or via the hub
	ModeDirectXe  = "direct"    // never use the hub (public-IP machines only)
)

// Settings is the frontend-facing settings shape.
type Settings struct {
	Label          string `json:"label"`
	MaxSessions    int    `json:"maxSessions"`
	MaxMbps        int    `json:"maxMbps"`
	ListenPort     int    `json:"listenPort"`
	BrokerURL      string `json:"brokerUrl"`
	HubAddress     string `json:"hubAddress"`
	ConnectionMode string `json:"connectionMode"`
}

// DirectSetupStatus is the platform's cached least-privilege TCP 443 setup
// state. It is separate from relay reachability: in Automatic mode, only the
// RelayHub nonce probe can confirm that the port is reachable from the
// Internet. Direct-only is an explicit operator override without that probe.
type DirectSetupStatus = directsetup.Status

// State is the single event payload the frontend renders from.
type State struct {
	Phase             string            `json:"phase"`
	Transport         string            `json:"transport"`
	RelayLabel        string            `json:"relayLabel"`
	RelayID           string            `json:"relayId"`
	PublicEndpoint    string            `json:"publicEndpoint"`
	LastError         *string           `json:"lastError"`
	StartedAtMs       int64             `json:"startedAtMs"`
	ActiveConnections int64             `json:"activeConnections"`
	TotalConnections  uint64            `json:"totalConnections"`
	BytesFromClients  uint64            `json:"bytesFromClients"`
	BytesToClients    uint64            `json:"bytesToClients"`
	LogLines          []string          `json:"logLines"`
	ConsentAccepted   bool              `json:"consentAccepted"`
	Running           bool              `json:"running"`
	XrayFound         bool              `json:"xrayFound"`
	Settings          Settings          `json:"settings"`
	DirectSetup       DirectSetupStatus `json:"directSetup"`
}

// Service is the Wails-bound bridge struct. Emitter must be assigned during
// app startup; the package never imports the Wails runtime (v2→v3 isolation,
// same rule as vpnservice).
type Service struct {
	Emitter func(State)

	// relayVersion is reported to the broker/hub. The component version comes
	// from desktop-volunteer/VERSION and is passed in by package main, keeping
	// the backend and frontend on one source of truth.
	relayVersion string

	// XrayPath points at the xray binary (bundled next to the app in packaged
	// builds). XrayFound reports whether resolution actually located one, so
	// the UI can warn before the first start attempt fails.
	XrayPath  string
	XrayFound bool

	mu       sync.Mutex
	settings persist.Settings
	engine   *engine.Engine
	ring     *ringBuffer
	dirty    bool
	store    *persist.Store
	stopEmit chan struct{}

	directSetup       directsetup.Controller
	directSetupStatus DirectSetupStatus
	directSetupOpMu   sync.Mutex

	// Platform setup inspections are external processes. Keep the read-only
	// deadline on the service so tests can exercise timeout behavior without
	// touching the developer machine's firewall or capabilities.
	setupInspectTimeout time.Duration
}

func New(componentVersion string) *Service {
	return NewWithDirectSetup(componentVersion, directsetup.NewManager())
}

// NewWithDirectSetup allows tests and alternate packagers to inject the
// platform setup boundary. Constructing the service never requests elevation.
func NewWithDirectSetup(componentVersion string, setup directsetup.Controller) *Service {
	if setup == nil {
		setup = directsetup.NewManager()
	}
	return &Service{
		ring:                newRingBuffer(logRingCapacity),
		relayVersion:        "desktop-volunteer/" + strings.TrimSpace(componentVersion),
		directSetup:         setup,
		directSetupStatus:   directsetup.InitialStatus(),
		setupInspectTimeout: directSetupInspectTimeout,
	}
}

// Startup and Shutdown take a context.Context so Wails cannot expose them to
// the frontend as callable bindings; they are lifecycle hooks for package main.
func (s *Service) Startup(ctx context.Context) {
	store, err := persist.New()
	if err == nil {
		s.mu.Lock()
		s.store = store
		s.settings = store.LoadSettings()
		s.mu.Unlock()
	} else {
		s.appendLog("settings unavailable: " + err.Error() + " (changes will not survive restarts)")
	}

	s.mu.Lock()
	if s.settings.Label == "" {
		s.settings.Label = relayruntime.GenerateLabel()
		s.persistSettingsLocked()
	}
	s.mu.Unlock()

	// Read-only and non-elevating. This detects app updates/replacements that
	// invalidated a Windows path-scoped rule or Linux file capability.
	s.refreshDirectSetupStatus(ctx)
	s.buildEngine()

	s.stopEmit = make(chan struct{})
	go s.emitLoop()
	s.emitCurrent()
}

func (s *Service) Shutdown(ctx context.Context) {
	if eng := s.currentEngine(); eng != nil {
		eng.Stop()
	}
	if s.stopEmit != nil {
		close(s.stopEmit)
	}
}

// buildEngine constructs the engine wired to this service's callbacks. The
// engine outlives individual runs (its traffic counters accumulate); it is
// rebuilt only here.
func (s *Service) buildEngine() {
	s.mu.Lock()
	defer s.mu.Unlock()

	var identity engine.Identity
	var configDir string
	if s.store != nil {
		identity = s.store.LoadIdentity()
		configDir = s.store.Dir()
	}

	cfg := s.engineConfigLocked()
	cfg.Identity = identity
	cfg.ConfigDir = configDir

	s.engine = engine.New(cfg, engine.Events{
		OnStatus: func(engine.Status) { s.emitCurrent() },
		OnIdentity: func(id engine.Identity) {
			s.mu.Lock()
			store := s.store
			s.mu.Unlock()
			if store != nil {
				if err := store.SaveIdentity(id); err != nil {
					s.appendLog("could not save relay identity: " + err.Error())
				}
			}
		},
		Log: s.logWriter(),
	})
}

// engineConfigLocked maps settings to an engine config. Mode is derived: a
// configured hub enables auto (probe picks direct or tunnel); without a hub
// only direct is possible.
func (s *Service) engineConfigLocked() engine.Config {
	settings := s.normalizedSettingsLocked()
	// Direct-only is a deliberate opt-out of the shared hub: the machine serves
	// on its own public address and never tunnels, so a hub outage cannot affect
	// it. Otherwise, a configured hub enables auto (probe picks direct or hub);
	// with no hub, direct is the only option.
	mode := engine.ModeDirect
	if settings.ConnectionMode != ModeDirectXe && settings.HubAddress != "" {
		mode = engine.ModeAuto
	}
	// The pinned fingerprint is for the built-in hub only. If the user points
	// the app at a different hub in Settings → Advanced, the pin would never
	// match, so drop it and fall back to that hub's own TLS trust.
	fingerprint := ""
	if settings.HubAddress == DefaultHubAddress {
		fingerprint = DefaultHubCertFingerprint
	}
	var automaticPortCandidates []int
	if mode == engine.ModeAuto {
		automaticPortCandidates = []int{directsetup.DirectPort}
		if settings.ListenPort != directsetup.DirectPort {
			automaticPortCandidates = append(automaticPortCandidates, settings.ListenPort)
		}
	}
	return engine.Config{
		BrokerURL:               settings.BrokerURL,
		Label:                   settings.Label,
		XrayPath:                s.XrayPath,
		ListenPort:              settings.ListenPort,
		AutomaticPortCandidates: automaticPortCandidates,
		Mode:                    mode,
		HubAddr:                 settings.HubAddress,
		HubCertFingerprint:      fingerprint,
		MaxSessions:             settings.MaxSessions,
		MaxMbps:                 settings.MaxMbps,
		Version:                 s.relayVersion,
		PunchCapable:            true,
	}
}

func (s *Service) normalizedSettingsLocked() Settings {
	out := Settings{
		Label:          s.settings.Label,
		MaxSessions:    s.settings.MaxSessions,
		MaxMbps:        s.settings.MaxMbps,
		ListenPort:     s.settings.ListenPort,
		BrokerURL:      s.settings.BrokerURL,
		HubAddress:     s.settings.HubAddress,
		ConnectionMode: s.settings.ConnectionMode,
	}
	if out.MaxSessions <= 0 {
		out.MaxSessions = relayruntime.DefaultMaxSessions
	}
	if out.MaxMbps <= 0 {
		out.MaxMbps = relayruntime.DefaultMaxMbps
	}
	if out.ListenPort <= 0 {
		out.ListenPort = defaultListenPort
	}
	if strings.TrimSpace(out.BrokerURL) == "" {
		out.BrokerURL = DefaultBrokerURL
	}
	if strings.TrimSpace(out.HubAddress) == "" {
		out.HubAddress = DefaultHubAddress
	}
	if out.ConnectionMode != ModeDirectXe {
		out.ConnectionMode = ModeAutomatic
	}
	return out
}

// Start begins volunteering. It refuses until the user has accepted the
// consent explanation (the relay makes this computer a visible traffic exit).
func (s *Service) Start() error {
	// Serialize relay startup with setup inspection/mutation. Wails methods may
	// be called concurrently; without this boundary, Start and Remove could
	// both observe an idle engine and then race the capability/firewall change.
	// Do not leave the Start binding wedged behind a slow platform command:
	// callers receive an actionable error and can retry after the current
	// inspection or explicit authorized change finishes.
	if !s.directSetupOpMu.TryLock() {
		return errors.New("local TCP 443 setup is being checked or changed; try starting again shortly")
	}
	defer s.directSetupOpMu.Unlock()

	s.mu.Lock()
	consent := s.settings.ConsentAccepted
	eng := s.engine
	directSetupStatus := s.directSetupStatus
	s.mu.Unlock()
	if !consent {
		return errors.New("consent required before volunteering")
	}
	if directSetupStatus.Reason == directsetup.ReasonRemovalRestartRequired {
		return errors.New("quit and reopen OpenRung Volunteer to complete TCP 443 capability removal before volunteering again")
	}
	if eng == nil {
		return errors.New("engine not initialized")
	}
	if !s.XrayFound {
		return errors.New("xray engine not found — reinstall OpenRung Volunteer")
	}

	// Apply the latest settings before starting; rejected (no-op) if already
	// running, which is fine — Start on a running engine is also a no-op.
	s.mu.Lock()
	cfg := s.engineConfigLocked()
	var identity engine.Identity
	if s.store != nil {
		identity = s.store.LoadIdentity()
		cfg.ConfigDir = s.store.Dir()
	}
	cfg.Identity = identity
	s.mu.Unlock()
	_ = eng.UpdateConfig(cfg)

	s.appendLog("starting relay…")
	if err := eng.Start(); err != nil {
		s.appendLog("start failed: " + err.Error())
		return err
	}
	s.emitCurrent()
	return nil
}

// Stop ends volunteering. It returns immediately; progress is reported through
// state events (stopping → idle). The relay disappears from the public
// directory once its broker lease expires (up to ~3 minutes).
func (s *Service) Stop() error {
	eng := s.currentEngine()
	if eng == nil {
		return nil
	}
	go func() {
		eng.Stop()
		s.appendLog("relay stopped")
		s.emitCurrent()
	}()
	s.emitCurrent()
	return nil
}

func (s *Service) GetState() State {
	return s.snapshot()
}

func (s *Service) GetSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.normalizedSettingsLocked()
}

// GetDirectSetupStatus refreshes the local OS status. It is read-only and
// never launches UAC, pkexec, sudo, or a privileged helper.
func (s *Service) GetDirectSetupStatus() DirectSetupStatus {
	status := s.refreshDirectSetupStatus(context.Background())
	s.emitCurrent()
	return status
}

// EnableDirectConnections is the sole UI-facing setup entry point. The
// frontend calls it only after a clear user action; depending on the platform
// it may show UAC or polkit authorization.
func (s *Service) EnableDirectConnections() (DirectSetupStatus, error) {
	s.directSetupOpMu.Lock()
	defer s.directSetupOpMu.Unlock()

	s.mu.Lock()
	setup := s.directSetup
	current := s.directSetupStatus
	eng := s.engine
	s.mu.Unlock()
	if setup == nil {
		status := directsetup.InitialStatus()
		return status, errors.New("direct TCP 443 setup is not initialized")
	}
	if eng != nil && eng.Running() {
		return current, errors.New("stop volunteering before changing local TCP 443 setup")
	}

	// The Manager independently bounds its read-only pre/post inspections.
	// Do not time out the outer authorization operation: on Windows the
	// unprivileged launcher cannot reliably cancel an already-elevated child,
	// so returning early could release serialization while the firewall is
	// still being changed.
	status, err := setup.Enable(context.Background())
	s.mu.Lock()
	s.directSetupStatus = status
	s.mu.Unlock()
	if err != nil {
		s.appendLog("direct TCP 443 setup was not enabled: " + err.Error())
	} else {
		s.appendLog("direct TCP 443 setup: " + status.Message)
	}
	s.emitCurrent()
	return status, err
}

// RemoveDirectConnections reverses only OpenRung's application-scoped local
// setup. It does not alter router, NAT, cloud, or unrelated firewall rules.
func (s *Service) RemoveDirectConnections() (DirectSetupStatus, error) {
	s.directSetupOpMu.Lock()
	defer s.directSetupOpMu.Unlock()

	s.mu.Lock()
	setup := s.directSetup
	current := s.directSetupStatus
	eng := s.engine
	s.mu.Unlock()
	if setup == nil {
		status := directsetup.InitialStatus()
		return status, errors.New("direct TCP 443 setup is not initialized")
	}
	if eng != nil && eng.Running() {
		return current, errors.New("stop volunteering before changing local TCP 443 setup")
	}

	// See EnableDirectConnections: inspection is bounded inside Manager, while
	// the privileged mutation remains serialized until its child has exited.
	status, err := setup.Remove(context.Background())
	s.mu.Lock()
	s.directSetupStatus = status
	s.mu.Unlock()
	if err != nil {
		s.appendLog("direct TCP 443 setup was not removed: " + err.Error())
	} else {
		s.appendLog("direct TCP 443 setup removal: " + status.Message)
	}
	s.emitCurrent()
	return status, err
}

// SaveSettings validates, persists, and returns the normalized settings. New
// settings apply on the next start; the UI disables editing while running.
func (s *Service) SaveSettings(in Settings) (Settings, error) {
	label := strings.TrimSpace(in.Label)
	if label != "" {
		// Reuse the relay-side normalization so the broker never rejects it.
		normalized, err := relay.NormalizeLabel(label)
		if err != nil {
			return Settings{}, fmt.Errorf("invalid relay name: %w", err)
		}
		label = normalized
	}
	if in.MaxSessions < 1 || in.MaxSessions > 4096 {
		return Settings{}, fmt.Errorf("max sessions must be between 1 and 4096")
	}
	if in.MaxMbps < 1 || in.MaxMbps > 10000 {
		return Settings{}, fmt.Errorf("max Mbps must be between 1 and 10000")
	}
	if in.ListenPort < 1 || in.ListenPort > 65535 {
		return Settings{}, fmt.Errorf("listen port must be between 1 and 65535")
	}
	connMode := strings.ToLower(strings.TrimSpace(in.ConnectionMode))
	if connMode != ModeDirectXe {
		connMode = ModeAutomatic
	}

	s.mu.Lock()
	s.settings.Label = label
	s.settings.MaxSessions = in.MaxSessions
	s.settings.MaxMbps = in.MaxMbps
	s.settings.ListenPort = in.ListenPort
	s.settings.BrokerURL = strings.TrimSpace(in.BrokerURL)
	s.settings.HubAddress = strings.TrimSpace(in.HubAddress)
	s.settings.ConnectionMode = connMode
	if s.settings.Label == "" {
		s.settings.Label = relayruntime.GenerateLabel()
	}
	s.persistSettingsLocked()
	out := s.normalizedSettingsLocked()
	s.mu.Unlock()

	s.emitCurrent()
	return out, nil
}

// RegenerateLabel replaces the public relay name with a fresh random one.
func (s *Service) RegenerateLabel() (string, error) {
	s.mu.Lock()
	s.settings.Label = relayruntime.GenerateLabel()
	s.persistSettingsLocked()
	label := s.settings.Label
	s.mu.Unlock()
	s.emitCurrent()
	return label, nil
}

// AcceptConsent records that the user has read and accepted the exit-relay
// explanation. It is a one-way latch persisted across launches.
func (s *Service) AcceptConsent() error {
	s.mu.Lock()
	s.settings.ConsentAccepted = true
	s.persistSettingsLocked()
	s.mu.Unlock()
	s.emitCurrent()
	return nil
}

// Running reports whether the relay engine is active (bound for the frontend;
// also used by main's quit confirmation).
func (s *Service) Running() bool {
	eng := s.currentEngine()
	return eng != nil && eng.Running()
}

func (s *Service) currentEngine() *engine.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

func (s *Service) refreshDirectSetupStatus(ctx context.Context) DirectSetupStatus {
	s.directSetupOpMu.Lock()
	defer s.directSetupOpMu.Unlock()

	s.mu.Lock()
	setup := s.directSetup
	current := s.directSetupStatus
	s.mu.Unlock()
	if setup == nil {
		return current
	}
	inspectCtx, cancel := context.WithTimeout(ctx, s.setupInspectTimeout)
	defer cancel()
	status := setup.Status(inspectCtx)
	s.mu.Lock()
	s.directSetupStatus = status
	s.mu.Unlock()
	return status
}

func (s *Service) persistSettingsLocked() {
	if s.store == nil {
		return
	}
	if err := s.store.SaveSettings(s.settings); err != nil {
		// Logged, not fatal: the session keeps the in-memory settings.
		go s.appendLog("could not save settings: " + err.Error())
	}
}

func (s *Service) snapshot() State {
	var engStatus engine.Status
	if eng := s.currentEngine(); eng != nil {
		engStatus = eng.Status()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var lastError *string
	if engStatus.LastError != "" {
		v := engStatus.LastError
		lastError = &v
	}
	phase := string(engStatus.Phase)
	if phase == "" {
		phase = string(engine.PhaseIdle)
	}
	label := engStatus.Label
	if label == "" {
		label = s.settings.Label
	}
	endpoint := ""
	if engStatus.PublicHost != "" {
		endpoint = formatPublicEndpoint(engStatus.PublicHost, engStatus.PublicPort)
	}

	return State{
		Phase:             phase,
		Transport:         engStatus.Transport,
		RelayLabel:        label,
		RelayID:           engStatus.RelayID,
		PublicEndpoint:    endpoint,
		LastError:         lastError,
		StartedAtMs:       engStatus.StartedAtMs,
		ActiveConnections: engStatus.ActiveConnections,
		TotalConnections:  engStatus.TotalConnections,
		BytesFromClients:  engStatus.BytesFromClients,
		BytesToClients:    engStatus.BytesToClients,
		LogLines:          s.ring.snapshot(),
		ConsentAccepted:   s.settings.ConsentAccepted,
		Running:           s.engine != nil && s.engine.Running(),
		XrayFound:         s.XrayFound,
		Settings:          s.normalizedSettingsLocked(),
		DirectSetup:       s.directSetupStatus,
	}
}

func formatPublicEndpoint(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (s *Service) appendLog(line string) {
	stamped := "[" + time.Now().Format("15:04:05") + "] " + line
	s.mu.Lock()
	s.ring.push(stamped)
	s.dirty = true
	s.mu.Unlock()
}

func (s *Service) emit(state State) {
	if s.Emitter != nil {
		s.Emitter(state)
	}
}

func (s *Service) emitCurrent() {
	s.emit(s.snapshot())
}

// emitLoop refreshes the frontend once per second while the relay runs (live
// counters) and flushes buffered log lines; status transitions emit
// immediately via emitCurrent.
func (s *Service) emitLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopEmit:
			return
		case <-ticker.C:
			s.mu.Lock()
			dirty := s.dirty
			s.dirty = false
			running := s.engine != nil && s.engine.Running()
			s.mu.Unlock()
			if dirty || running {
				s.emitCurrent()
			}
		}
	}
}

// logWriter adapts the log ring to an io.Writer for the engine and xray.
type logWriter struct{ s *Service }

func (s *Service) logWriter() *logWriter { return &logWriter{s: s} }

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			w.s.appendLog(line)
		}
	}
	return len(p), nil
}
