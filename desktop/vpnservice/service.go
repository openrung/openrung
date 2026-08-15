// Package vpnservice exposes the desktop VPN engine to the webview. The bound
// method surface mirrors the mobile native bridge contract
// (openrung-mobile-app/src/native/types.ts, docs/CONTRACT.md §3) so the mobile
// state layer ports to desktop unchanged.
//
// The engine itself — state machine, connect ladder, ranking, WSS fallback,
// punch attempt, health monitoring, directory cache — lives in
// openrung/internal/connectcore (docs/adr/001). This package is the thin
// desktop adapter over it: Wails event emission with the log ring and its
// coalescing, plus the desktop/persist, desktop/proxymode, and
// desktop/proxyconfig wiring behind the engine's platform interfaces.
package vpnservice

import (
	"context"
	"runtime"
	"sync"
	"time"

	"openrung/desktop/persist"
	"openrung/desktop/proxyconfig"
	"openrung/desktop/proxymode"
	"openrung/internal/clienttelemetry"
	"openrung/internal/connectcore"
	"openrung/internal/relay"
)

type ConnectionStatus string

const (
	StatusDisconnected  ConnectionStatus = ConnectionStatus(connectcore.StatusDisconnected)
	StatusPreparing     ConnectionStatus = ConnectionStatus(connectcore.StatusPreparing)
	StatusConnecting    ConnectionStatus = ConnectionStatus(connectcore.StatusConnecting)
	StatusConnected     ConnectionStatus = ConnectionStatus(connectcore.StatusConnected)
	StatusDisconnecting ConnectionStatus = ConnectionStatus(connectcore.StatusDisconnecting)
	StatusFailed        ConnectionStatus = ConnectionStatus(connectcore.StatusFailed)
)

const logRingCapacity = 80

type RecentNode struct {
	CountryCode string  `json:"countryCode"`
	Label       string  `json:"label"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

type NativeVpnState struct {
	Status     ConnectionStatus `json:"status"`
	RelayLabel *string          `json:"relayLabel"`
	LastError  *string          `json:"lastError"`
	LogLines   []string         `json:"logLines"`
	Recents    []RecentNode     `json:"recents"`
}

type NativeIdentity struct {
	ClientID  string  `json:"clientId"`
	SessionID *string `json:"sessionId"`
}

// NativeProxyInfo is desktop-specific connection metadata, kept separate from
// NativeVpnState so that state remains identical to the shared mobile bridge
// contract. The helper commands are intended to be copied into a POSIX shell.
type NativeProxyInfo struct {
	Host                  string  `json:"host"`
	Port                  int     `json:"port"`
	Endpoint              string  `json:"endpoint"`
	PersistenceWarning    *string `json:"persistenceWarning"`
	ShellIntegration      bool    `json:"shellIntegration"`
	ShellIntegrationError *string `json:"shellIntegrationError"`
	HelperPath            string  `json:"helperPath"`
	EnableCommand         string  `json:"enableCommand"`
	DisableCommand        string  `json:"disableCommand"`
}

// clientID resolves the stable per-install identifier. It is a package var so
// tests can stub it; it wraps clienttelemetry.ClientID, which persists to
// os.UserConfigDir()/openrung/client-id with correct per-OS paths.
var clientID = clienttelemetry.ClientID

// Service is the Wails-bound bridge struct. Emitter must be assigned during app
// startup, before the frontend can invoke any bound method; vpnservice never
// imports the Wails runtime so a future v2→v3 migration stays confined to
// package main.
type Service struct {
	Emitter func(NativeVpnState)

	// SingBoxPath overrides the sing-box binary path (defaults to "sing-box"
	// resolved via PATH). Packaging points this at the bundled binary; it is
	// forwarded to the engine in Startup, before any bound method can connect.
	SingBoxPath string

	engine *connectcore.Engine
	store  *persist.Store

	// mu guards the log ring, the engine-state mirror, and the dirty flag. The
	// sink updates the mirror synchronously under the engine's own state lock,
	// so emits stay ordered (see engineSink); nothing here may call back into
	// the engine while holding mu, or the two locks would deadlock.
	mu       sync.Mutex
	state    connectcore.State
	ring     *ringBuffer
	dirty    bool
	stopEmit chan struct{}
}

func New() *Service {
	s := &Service{ring: newRingBuffer(logRingCapacity)}
	engine := connectcore.New()
	engine.Sink = &engineSink{s: s}
	engine.OSProxy = osProxyAdapter{ctrl: proxymode.New()}
	engine.ResolveProxyPort = func() (connectcore.ProxyPortResolution, error) {
		resolution, err := proxyconfig.ResolvePort(s.store)
		if err != nil {
			return connectcore.ProxyPortResolution{}, err
		}
		return connectcore.ProxyPortResolution{
			Port:               resolution.Port,
			PersistenceWarning: resolution.PersistenceWarning,
		}, nil
	}
	s.engine = engine
	s.state = engine.State()
	return s
}

// Startup and Shutdown take a context.Context so Wails cannot expose them to the
// frontend as callable bindings; they are lifecycle hooks for package main.
func (s *Service) Startup(ctx context.Context) {
	s.engine.SingBoxPath = s.SingBoxPath
	if store, err := persist.New(); err == nil {
		s.store = store
		s.engine.Persistence = storeAdapter{store: store}
	}
	// Crash recovery and persisted recents; the engine's log lines arrive
	// through the sink.
	s.engine.Start()
	// Resolve the stable endpoint and generate its sourceable shell helper even
	// while disconnected, so Settings can expose it immediately. Failure stays
	// non-fatal here; Connect/GetProxyInfo surface the actionable error.
	if info, err := s.GetProxyInfo(); err != nil {
		s.appendLog("could not prepare local proxy configuration: " + err.Error())
	} else if info.ShellIntegrationError != nil {
		s.appendLog("could not prepare proxy shell helper: " + *info.ShellIntegrationError)
	}
	if warning := s.engine.LocalProxyPortWarning(); warning != nil {
		s.appendLog(warning.Error())
	}
	s.stopEmit = make(chan struct{})
	go s.emitLoop()
	s.emitCurrent()
}

func (s *Service) Shutdown(ctx context.Context) {
	s.engine.Stop()
	if s.stopEmit != nil {
		close(s.stopEmit)
	}
}

// Prepare mirrors the mobile bridge's OS-consent step; the engine owns the
// answer (proxy mode needs none, TUN will elevate).
func (s *Service) Prepare() (bool, error) {
	return s.engine.Prepare()
}

// Connect starts (or switches) the tunnel. targetRelayID takes precedence over
// targetCountry; empty strings stand in for the contract's nulls. It resolves
// once the start has been dispatched — completion is reported via events.
func (s *Service) Connect(brokerURL, targetCountry, targetRelayID string) error {
	return s.engine.Connect(brokerURL, targetCountry, targetRelayID)
}

func (s *Service) Disconnect() error {
	return s.engine.Disconnect()
}

func (s *Service) GetState() NativeVpnState {
	// Read the engine before taking s.mu (never the other way around — see mu).
	state := s.engine.State()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeStateLocked(state)
}

func (s *Service) GetIdentity() NativeIdentity {
	id, err := clientID()
	if err != nil {
		id = ""
	}
	sessionID := s.engine.SessionID()
	var session *string
	if sessionID != "" {
		session = &sessionID
	}
	return NativeIdentity{ClientID: id, SessionID: session}
}

// GetProxyInfo returns the stable loopback endpoint and copyable shell helper
// commands. Sourcing the helper is explicit because a GUI process cannot
// mutate an already-running parent shell's environment.
func (s *Service) GetProxyInfo() (NativeProxyInfo, error) {
	port, err := s.engine.LocalProxyPort()
	if err != nil {
		return NativeProxyInfo{}, err
	}
	info, err := proxyconfig.EndpointInfo(port)
	if err != nil {
		return NativeProxyInfo{}, err
	}
	native := NativeProxyInfo{
		Host:             info.Host,
		Port:             info.Port,
		Endpoint:         info.Endpoint,
		ShellIntegration: runtime.GOOS != "windows",
	}
	if warning := s.engine.LocalProxyPortWarning(); warning != nil {
		message := warning.Error()
		native.PersistenceWarning = &message
	}
	if !native.ShellIntegration {
		return native, nil
	}
	info, err = proxyconfig.WriteShellHelper(s.store, port)
	if err != nil {
		message := err.Error()
		native.ShellIntegrationError = &message
		return native, nil
	}
	native.HelperPath = info.HelperPath
	native.EnableCommand = info.EnableCommand
	native.DisableCommand = info.DisableCommand
	return native, nil
}

// ListRelaysForDirectory is Wails-bound. It returns the broker's relay list for
// the frontend to aggregate into map regions (the TS loadExitNodeDirectory,
// ported from mobile, does the grouping).
func (s *Service) ListRelaysForDirectory() (relay.ListResponse, error) {
	return s.engine.ListRelaysForDirectory()
}

// ---- engine event sink + emission ----

// engineSink adapts engine events onto the Wails surface: a state change is
// emitted immediately (merged with the log ring), while a log line only marks
// the ring dirty for the coalescing loop below.
type engineSink struct{ s *Service }

// StateChanged runs under the engine's state lock, so updating the mirror and
// emitting under s.mu keeps the engine's write order: the last status writer
// is also the last to emit, and the frontend never ends on a status a later
// write already superseded.
func (k *engineSink) StateChanged(state connectcore.State) {
	s := k.s
	s.mu.Lock()
	s.state = state
	s.emit(s.snapshotLocked())
	s.mu.Unlock()
}

func (k *engineSink) Log(entry connectcore.LogEntry) {
	k.s.appendLogEntry(entry)
}

func (s *Service) appendLog(line string) {
	s.appendLogEntry(connectcore.LogEntry{Time: time.Now(), Line: line})
}

func (s *Service) appendLogEntry(entry connectcore.LogEntry) {
	stamped := "[" + entry.Time.Format("15:04:05") + "] " + entry.Line
	s.mu.Lock()
	s.ring.push(stamped)
	s.dirty = true
	s.mu.Unlock()
}

func (s *Service) snapshotLocked() NativeVpnState {
	return s.nativeStateLocked(s.state)
}

// nativeStateLocked merges an engine state with the log ring into the wire
// shape the frontend consumes. The caller holds s.mu.
func (s *Service) nativeStateLocked(state connectcore.State) NativeVpnState {
	recents := make([]RecentNode, 0, len(state.Recents))
	for _, r := range state.Recents {
		recents = append(recents, RecentNode(r))
	}
	return NativeVpnState{
		Status:     ConnectionStatus(state.Status),
		RelayLabel: state.RelayLabel,
		LastError:  state.LastError,
		LogLines:   s.ring.snapshot(),
		Recents:    recents,
	}
}

func (s *Service) emit(snap NativeVpnState) {
	if s.Emitter != nil {
		s.Emitter(snap)
	}
}

// emitCurrent refreshes the mirror from the engine and emits. It runs once at
// the end of Startup — before any connect exists — so the startup-loaded
// recents reach the mirror without racing a concurrent engine write.
func (s *Service) emitCurrent() {
	state := s.engine.State()
	s.mu.Lock()
	s.state = state
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.emit(snap)
}

// emitLoop coalesces high-frequency log updates: status transitions emit
// immediately, but a burst of sing-box log lines only sets a dirty flag that is
// flushed at 5 Hz, so a chatty tunnel can't flood the webview.
func (s *Service) emitLoop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopEmit:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.dirty {
				s.mu.Unlock()
				continue
			}
			s.dirty = false
			// Emit under the lock so this coalesced log flush is ordered against
			// the status emits (which also run under s.mu): otherwise a stale
			// snapshot captured here could be delivered after a terminal status
			// write, leaving the UI on a superseded transient status.
			s.emit(s.snapshotLocked())
			s.mu.Unlock()
		}
	}
}
