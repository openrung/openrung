// Package vpnservice exposes the desktop VPN engine to the webview. The bound
// method surface mirrors the mobile native bridge contract
// (openrung-mobile-app/src/native/types.ts, docs/CONTRACT.md §3) so the mobile
// state layer ports to desktop unchanged.
package vpnservice

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/desktop/persist"
	"openrung/desktop/proxymode"
	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/punch"
)

type ConnectionStatus string

const (
	StatusDisconnected  ConnectionStatus = "disconnected"
	StatusPreparing     ConnectionStatus = "preparing"
	StatusConnecting    ConnectionStatus = "connecting"
	StatusConnected     ConnectionStatus = "connected"
	StatusDisconnecting ConnectionStatus = "disconnecting"
	StatusFailed        ConnectionStatus = "failed"
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

// clientID resolves the stable per-install identifier. It is a package var so
// tests can stub it; it wraps clienttelemetry.ClientID, which persists to
// os.UserConfigDir()/openrung/client-id with correct per-OS paths.
var clientID = clienttelemetry.ClientID

// coreState is the mutable slice of NativeVpnState the service owns directly.
// LogLines live in the ring and are merged in at snapshot time.
type coreState struct {
	status     ConnectionStatus
	relayLabel *string
	lastError  *string
	recents    []RecentNode
}

// connection tracks one active connect goroutine so Disconnect (and a switch)
// can cancel it and so cleanup runs exactly once on exit.
type connection struct {
	cancel        context.CancelFunc
	done          chan struct{}
	disconnecting bool // set under mu before cancel: a clean stop, not a crash
	proxySet      bool
	snapshot      proxymode.Snapshot
	configPath    string
	punch         *punch.Establishment // live punched path, nil when using the hub
	mgr           *clienttelemetry.Manager
}

// Service is the Wails-bound bridge struct. Emitter must be assigned during app
// startup, before the frontend can invoke any bound method; vpnservice never
// imports the Wails runtime so a future v2→v3 migration stays confined to
// package main.
type Service struct {
	Emitter func(NativeVpnState)

	// SingBoxPath overrides the sing-box binary path (defaults to "sing-box"
	// resolved via PATH). Packaging points this at the bundled binary.
	SingBoxPath string

	// PunchEnabled attempts a direct NAT-punched path to punch-capable
	// volunteers before falling back to the relay hub's data plane.
	// PunchInsecure skips TLS verification of the hub's self-signed punch
	// coordination endpoint (volunteer hubs on bare IPs cannot get a CA cert);
	// see punchHTTPClient for why that stays safe.
	PunchEnabled  bool
	PunchInsecure bool

	mu        sync.Mutex
	core      coreState
	sessionID string
	ring      *ringBuffer
	dirty     bool
	conn      *connection

	directory *directoryCache
	store     *persist.Store
	proxy     proxymode.Controller
	stopEmit  chan struct{}
}

func New() *Service {
	return &Service{
		core:          coreState{status: StatusDisconnected},
		ring:          newRingBuffer(logRingCapacity),
		directory:     newDirectoryCache(),
		proxy:         proxymode.New(),
		PunchEnabled:  true,
		PunchInsecure: true,
	}
}

// Startup and Shutdown take a context.Context so Wails cannot expose them to the
// frontend as callable bindings; they are lifecycle hooks for package main.
func (s *Service) Startup(ctx context.Context) {
	if store, err := persist.New(); err == nil {
		s.store = store
		// Crash recovery: a leftover proxy snapshot means a prior session died
		// without restoring the OS proxy. Undo it before doing anything else.
		if snap, ok := store.LoadProxySnapshot(); ok {
			_ = s.proxy.Restore(snap)
			_ = store.ClearProxySnapshot()
		}
		recents := toRecentNodes(store.LoadRecents())
		s.mu.Lock()
		s.core.recents = recents
		s.mu.Unlock()
	}
	s.stopEmit = make(chan struct{})
	go s.emitLoop()
	s.emitCurrent()
}

func (s *Service) Shutdown(ctx context.Context) {
	// Tear down any live tunnel so the OS proxy is restored on quit.
	s.teardownExisting()
	if s.stopEmit != nil {
		close(s.stopEmit)
	}
}

// Prepare mirrors the mobile bridge's OS-consent step. Proxy mode needs no OS
// consent on desktop; TUN mode will perform the elevation handshake here.
func (s *Service) Prepare() (bool, error) {
	return true, nil
}

// Connect starts (or switches) the tunnel. targetRelayID takes precedence over
// targetCountry; empty strings stand in for the contract's nulls. It resolves
// once the start has been dispatched — completion is reported via events.
func (s *Service) Connect(brokerURL, targetCountry, targetRelayID string) error {
	s.teardownExisting()

	ctx, cancel := context.WithCancel(context.Background())
	conn := &connection{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	s.setStatus(StatusPreparing, keepLabel, clearError)
	go s.runConnect(ctx, conn, brokerURL, targetCountry, targetRelayID)
	return nil
}

func (s *Service) Disconnect() error {
	s.mu.Lock()
	conn := s.conn
	if conn == nil {
		s.mu.Unlock()
		return nil
	}
	conn.disconnecting = true
	s.mu.Unlock()

	s.setStatus(StatusDisconnecting, keepLabel, keepError)
	conn.cancel() // runConnect's supervisor finalizes state + proxy restore
	return nil
}

func (s *Service) GetState() NativeVpnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) GetIdentity() NativeIdentity {
	id, err := clientID()
	if err != nil {
		id = ""
	}
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	var session *string
	if sessionID != "" {
		session = &sessionID
	}
	return NativeIdentity{ClientID: id, SessionID: session}
}

// runConnect is the connect flow, ported from cmd/client/main.go runConnect but
// building a proxy-mode config and owning the full teardown on exit.
func (s *Service) runConnect(ctx context.Context, conn *connection, brokerURL, targetCountry, targetRelayID string) {
	defer close(conn.done)

	s.setStatus(StatusConnecting, keepLabel, clearError)

	mgr := newManager(brokerURL)
	conn.mgr = mgr
	if mgr != nil {
		if session, err := mgr.BeginSession(); err == nil && session != nil {
			s.mu.Lock()
			s.sessionID = session.ID
			s.mu.Unlock()
		}
		mgr.Record("connection_attempted", "", nil, nil)
	}
	sessionID := s.currentSessionID()

	fetch, err := discovery.FirstReachable(ctx, config.BrokerCandidates(brokerURL), discovery.Options{
		Limit:     config.RelayLimit,
		ClientID:  managerClientID(mgr),
		SessionID: sessionID,
	})
	if err != nil {
		s.failConnect(conn, "broker_fetch", err)
		return
	}

	selected, err := selectRelay(fetch.Response, targetCountry, targetRelayID)
	if err != nil {
		s.failConnect(conn, "relay_select", err)
		return
	}

	port, err := freeLoopbackPort()
	if err != nil {
		s.failConnect(conn, "proxy_port", err)
		return
	}

	// Try a direct NAT-punched path first; on any failure fall back to the relay
	// hub endpoint so the outcome is never worse than not punching.
	configInput := client.SingBoxConfigInput{
		Relay:           selected,
		Mode:            client.ModeProxy,
		ProxyListenPort: port,
	}
	if est := s.maybePunch(ctx, conn.mgr, selected); est != nil {
		conn.punch = est
		configInput.BridgeHost = est.BridgeHost
		configInput.BridgePort = est.BridgePort
		configInput.PunchPeerExcludeAddress = est.PeerIP
		go func() { _ = est.Bridge.Serve(ctx) }()
		s.appendLog(fmt.Sprintf("punched direct path to %s (peer %s, nat %s)", selected.ID, est.PeerIP, est.NATClass))
	}

	configJSON, err := client.BuildSingBoxConfig(configInput)
	if err != nil {
		s.failConnect(conn, "config_build", err)
		return
	}
	configPath, err := writeTempConfig(configJSON)
	if err != nil {
		s.failConnect(conn, "config_write", err)
		return
	}
	conn.configPath = configPath

	s.applySystemProxy(conn, port)
	s.appendLog(fmt.Sprintf("proxy listening on 127.0.0.1:%d", port))

	runner := client.SingBoxRunner{Path: s.SingBoxPath, Stdout: s.logWriter(), Stderr: s.logWriter()}
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- runner.Run(ctx, configPath) }()

	// If sing-box dies immediately, the tunnel never came up.
	select {
	case runErr := <-runErrCh:
		s.failConnect(conn, "tunnel_start", runErr)
		return
	case <-time.After(400 * time.Millisecond):
	}

	label := geoLabel(selected)
	s.markConnected(label, recentFrom(selected))
	if mgr != nil {
		mgr.MarkConnected(selected.ID)
		mgr.Record("connection_succeeded", selected.ID, nil, nil)
		_ = mgr.Flush(ctx)
		go mgr.RunHeartbeatLoop(ctx)
	}

	runErr := <-runErrCh
	s.finishConnect(conn, selected.ID, runErr)
}

// applySystemProxy snapshots and points the OS proxy at the local mixed inbound.
// Failure is non-fatal: sing-box still listens on loopback, so the app can fall
// back to advertising a manual proxy address.
func (s *Service) applySystemProxy(conn *connection, port int) {
	if !s.proxy.Supported() {
		s.appendLog(fmt.Sprintf("system proxy unavailable here; set manual proxy 127.0.0.1:%d", port))
		return
	}
	snap, err := s.proxy.Snapshot()
	if err != nil {
		s.appendLog("could not read current system proxy; leaving it unchanged")
		return
	}
	if s.store != nil {
		_ = s.store.SaveProxySnapshot(snap) // persist for crash recovery
	}
	if err := s.proxy.Set("127.0.0.1", port); err != nil {
		s.appendLog(fmt.Sprintf("system proxy set failed; set manual proxy 127.0.0.1:%d", port))
		if s.store != nil {
			_ = s.store.ClearProxySnapshot()
		}
		return
	}
	conn.proxySet = true
	conn.snapshot = snap
}

// cleanupConn restores the OS proxy, clears the persisted snapshot, and removes
// the temp config. Safe to call once per connection on exit.
func (s *Service) cleanupConn(conn *connection) {
	// Close the punched path first: sing-box has already exited by the time
	// cleanup runs, so the bridge has no more readers.
	if conn.punch != nil {
		_ = conn.punch.Close()
	}
	if conn.proxySet {
		_ = s.proxy.Restore(conn.snapshot)
	}
	if s.store != nil {
		_ = s.store.ClearProxySnapshot()
	}
	if conn.configPath != "" {
		_ = os.Remove(conn.configPath)
	}
}

func (s *Service) failConnect(conn *connection, stage string, err error) {
	s.cleanupConn(conn)
	msg := fmt.Sprintf("%s: %v", stage, err)
	s.appendLog("connect failed: " + msg)
	s.setStatus(StatusFailed, keepLabel, setError(msg))
	if conn.mgr != nil {
		conn.mgr.Record("connection_failed", "", map[string]string{"failure_stage": stage}, nil)
		conn.mgr.EndSession("connection_failed")
		flushOnShutdown(conn.mgr)
	}
	s.clearConn(conn)
}

func (s *Service) finishConnect(conn *connection, relayID string, runErr error) {
	s.mu.Lock()
	disconnecting := conn.disconnecting
	s.mu.Unlock()

	s.cleanupConn(conn)
	if conn.mgr != nil {
		conn.mgr.Record("tunnel_stopped", relayID, nil, nil)
	}

	if disconnecting {
		s.appendLog("disconnected")
		s.setStatus(StatusDisconnected, clearLabel, clearError)
		endSession(conn.mgr, "disconnect")
	} else {
		// sing-box exited on its own: a crash, not a user disconnect.
		msg := "tunnel exited unexpectedly"
		if runErr != nil {
			msg = runErr.Error()
		}
		s.appendLog("tunnel stopped: " + msg)
		s.setStatus(StatusFailed, keepLabel, setError(msg))
		endSession(conn.mgr, "tunnel_exited")
	}
	s.clearConn(conn)
}

// teardownExisting cancels any active connection and waits for its supervisor to
// finish cleanup, so a switch or shutdown never races two connections.
func (s *Service) teardownExisting() {
	s.mu.Lock()
	conn := s.conn
	if conn != nil {
		conn.disconnecting = true
	}
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.cancel()
	<-conn.done
}

func (s *Service) clearConn(conn *connection) {
	s.mu.Lock()
	if s.conn == conn {
		s.conn = nil
	}
	s.sessionID = ""
	s.mu.Unlock()
}

// ---- state mutation + emit ----

type labelOp int

const (
	keepLabel labelOp = iota
	clearLabel
)

type errorOp struct {
	clear bool
	set   bool
	value string
}

var (
	keepError  = errorOp{}
	clearError = errorOp{clear: true}
)

func setError(msg string) errorOp { return errorOp{set: true, value: msg} }

func (s *Service) setStatus(status ConnectionStatus, label labelOp, errOp errorOp) {
	s.mu.Lock()
	s.core.status = status
	if label == clearLabel {
		s.core.relayLabel = nil
	}
	switch {
	case errOp.clear:
		s.core.lastError = nil
	case errOp.set:
		v := errOp.value
		s.core.lastError = &v
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.emit(snap)
}

func (s *Service) markConnected(label string, recent *RecentNode) {
	s.appendLog("connected via " + label)
	s.mu.Lock()
	s.core.status = StatusConnected
	l := label
	s.core.relayLabel = &l
	s.core.lastError = nil
	if recent != nil {
		s.core.recents = persistPrepend(s.store, s.core.recents, *recent)
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.emit(snap)
}

func (s *Service) appendLog(line string) {
	stamped := "[" + time.Now().Format("15:04:05") + "] " + line
	s.mu.Lock()
	s.ring.push(stamped)
	s.dirty = true
	s.mu.Unlock()
}

func (s *Service) snapshotLocked() NativeVpnState {
	return NativeVpnState{
		Status:     s.core.status,
		RelayLabel: s.core.relayLabel,
		LastError:  s.core.lastError,
		LogLines:   s.ring.snapshot(),
		Recents:    append([]RecentNode{}, s.core.recents...),
	}
}

func (s *Service) emit(snap NativeVpnState) {
	if s.Emitter != nil {
		s.Emitter(snap)
	}
}

func (s *Service) emitCurrent() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.emit(snap)
}

func (s *Service) currentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
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
			snap := s.snapshotLocked()
			s.mu.Unlock()
			s.emit(snap)
		}
	}
}

// logWriter adapts the service's log ring to an io.Writer for SingBoxRunner.
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
