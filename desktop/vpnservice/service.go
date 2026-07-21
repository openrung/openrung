// Package vpnservice exposes the desktop VPN engine to the webview. The bound
// method surface mirrors the mobile native bridge contract
// (openrung-mobile-app/src/native/types.ts, docs/CONTRACT.md §3) so the mobile
// state layer ports to desktop unchanged.
package vpnservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/desktop/persist"
	"openrung/desktop/proxyconfig"
	"openrung/desktop/proxymode"
	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/punch"
	"openrung/internal/relay"
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
	finalized     bool // set under mu once finalizeConn owns the terminal status
	proxySet      bool // OS proxy may differ from the snapshot and still needs restore
	snapshotTaken bool // snapshot captured once; survives a recovery proxy release
	snapshot      proxymode.Snapshot
	mgr           *clienttelemetry.Manager

	// active is the promoted (live) candidate's resources; nil while the ladder
	// is still trying candidates or after a teardown. Only the runConnect
	// goroutine assigns and tears it down; mu guards the pointer for readers.
	active *candidateResult
	// candidates is the last fetched usable+filtered list in ladder order —
	// client-latency ranked, with a recovery's failed relay demoted last. A
	// recovery re-ladder replaces it.
	candidates    []relay.Descriptor
	activeRelayID string
	brokerURL     string // the front that served this session's fetch (health-monitor liveness reference)
	// wssTicketRetryUsed permits at most one bounded all-front Retry-After wait
	// per ladder pass, rather than one sleep for every relay and front.
	wssTicketRetryUsed bool
	// heartbeatOnce starts the telemetry heartbeat loop at most once per
	// session, however many times a recovery re-ladder promotes a new relay.
	heartbeatOnce sync.Once
}

// candidateResult owns one connect-ladder candidate's live resources and the
// measurements that feed connection_succeeded. teardown releases the resources
// in the pinned order and is idempotent.
type candidateResult struct {
	relay relay.Descriptor
	// accessTransport is the client-to-relay path, distinct from the relay's
	// registration transport. frontID is set only for relay-local WSS fallback.
	accessTransport string
	frontID         string
	ctx             context.Context
	cancel          context.CancelFunc
	runErrCh        chan error
	reaped          bool // runErrCh already drained (the process is reaped)
	torndown        bool
	punch           *punch.Establishment // live punched path, nil when using the hub

	// The WSS adapter remains alive until sing-box has been cancelled and reaped.
	// Its separate context preserves that teardown order.
	wssBridge    wssBridge
	wssDone      chan struct{}
	wssCancel    context.CancelFunc
	transportErr chan error
	configPath   string
	proxyPort    int
	tcpMS        int64
	hasTCPMS     bool
	transportMS  int64
	startMS      int64
	probeMS      int64
	attempt      int64 // 1-based index in the ladder that produced it
	// brokerIndex is where this relay sat in the broker's order before client
	// ranking reordered the ladder; -1 until ladderOrder.annotate stamps it, so
	// an unannotated result never claims it was the broker's first choice.
	brokerIndex int64
	// rankProbeMS is the ranker's measured TCP latency, nil when this relay was
	// not probed or its probe failed.
	rankProbeMS *int64
}

// localCandidateError marks failures independent of the selected relay path:
// config generation, temp state, sing-box startup/early exit, and local inbound
// readiness. Retrying a relay or minting a ticket cannot repair them.
type localCandidateError struct {
	stage string
	err   error
}

func (e *localCandidateError) Error() string { return e.err.Error() }
func (e *localCandidateError) Unwrap() error { return e.err }

func markLocalCandidateError(stage string, err error) error {
	if err == nil {
		err = errors.New("local VPN setup failed")
	}
	return &localCandidateError{stage: stage, err: err}
}

func localCandidateErrorStage(err error) (string, bool) {
	var localErr *localCandidateError
	if !errors.As(err, &localErr) {
		return "", false
	}
	return localErr.stage, true
}

// teardown releases a candidate's resources in the pinned order: cancel the
// candidate context, reap sing-box, close the punched path (only after the
// process exits — the bridge must not close while sing-box could still read
// it), remove the temp config. Safe to call more than once and on nil.
func (c *candidateResult) teardown() {
	if c == nil || c.torndown {
		return
	}
	c.torndown = true
	if c.cancel != nil {
		c.cancel()
	}
	if c.runErrCh != nil && !c.reaped {
		<-c.runErrCh
		c.reaped = true
	}
	if c.punch != nil {
		_ = c.punch.Close()
	}
	if c.wssBridge != nil {
		if c.wssCancel != nil {
			c.wssCancel()
		}
		_ = c.wssBridge.Close()
	}
	if c.wssDone != nil {
		<-c.wssDone
	}
	if c.configPath != "" {
		_ = os.Remove(c.configPath)
	}
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
	// relays before falling back to the relay hub's data plane.
	// PunchInsecure skips TLS verification of the hub's self-signed punch
	// coordination endpoint (relay hubs on bare IPs cannot get a CA cert);
	// see punchHTTPClient for why that stays safe.
	PunchEnabled  bool
	PunchInsecure bool

	// connectMu serializes the Connect/Disconnect mutation surface. Wails
	// dispatches every bound call on its own goroutine, so without this two
	// overlapping Connects could both pass teardownExisting and orphan a live
	// connection whose supervisor keeps a tunnel alive forever. mu still guards
	// the finer-grained fields; connectMu only orders whole connect/disconnect
	// operations.
	connectMu sync.Mutex

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

	// proxyPortMu pins only a successfully resolved endpoint for this process.
	// A transient allocation failure remains retryable on the next Settings or
	// Connect call. ResolvePort persists automatic selections across launches.
	proxyPortMu   sync.Mutex
	proxyPort     int
	proxyPortWarn error

	// Test seams (nil means the production implementation). They mirror the
	// proxy-controller injection pattern above so ladder tests need no network,
	// no broker, and no sing-box binary.
	runTunnel         func(ctx context.Context, configPath string) error
	probeTunnel       func(ctx context.Context, proxyPort int) (int64, error)
	healthProbe       func(ctx context.Context, proxyPort int) error
	dialRelay         func(ctx context.Context, host string, port int) (int64, error)
	fetchRelays       func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error)
	tunnelReady       func(ctx context.Context, proxyPort int) error
	writeConfig       func(data []byte) (string, error)
	requestWSSTicket  func(ctx context.Context, brokerURL string, request relay.WSSSessionTicketRequest, clientID, sessionID string) (relay.WSSSessionTicketResponse, error)
	dialWSS           func(ctx context.Context, rawURL, ticket string) (wssBridge, error)
	waitWSSRetry      func(ctx context.Context, delay time.Duration) error
	checkNetworkAlive func(ctx context.Context, fronts []string) bool
	healthTick        time.Duration // 0 means config.HealthProbeInterval
	networkRetryDelay time.Duration // 0 means networkRecoveryPollInterval
	tunnelReadyLimit  time.Duration // 0 means config.TunnelReadyTimeout
}

func (s *Service) candidateConfigWriter() func([]byte) (string, error) {
	if s.writeConfig != nil {
		return s.writeConfig
	}
	return writeTempConfig
}

func (s *Service) tunnelReadyProbe() func(context.Context, int) error {
	if s.tunnelReady != nil {
		return s.tunnelReady
	}
	return loopbackReady
}

func (s *Service) tunnelRunner() func(context.Context, string) error {
	if s.runTunnel != nil {
		return s.runTunnel
	}
	return func(ctx context.Context, configPath string) error {
		runner := client.SingBoxRunner{
			Path:      s.SingBoxPath,
			Stdout:    s.logWriter(),
			Stderr:    s.logWriter(),
			KillGrace: config.LadderKillGrace,
		}
		return runner.Run(ctx, configPath)
	}
}

func (s *Service) tunnelProber() func(context.Context, int) (int64, error) {
	if s.probeTunnel != nil {
		return s.probeTunnel
	}
	return verifyInternetViaProxy
}

func (s *Service) healthProber() func(context.Context, int) error {
	if s.healthProbe != nil {
		return s.healthProbe
	}
	return healthSweepViaProxy
}

func (s *Service) relayDialer() func(context.Context, string, int) (int64, error) {
	if s.dialRelay != nil {
		return s.dialRelay
	}
	return relayTCPReachable
}

func (s *Service) relayFetcher() func(context.Context, string, int, string, string) (discovery.Fetch, error) {
	if s.fetchRelays != nil {
		return s.fetchRelays
	}
	return func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		return discovery.FirstReachable(ctx, config.BrokerCandidates(brokerURL), discovery.Options{
			Limit:     limit,
			ClientID:  clientID,
			SessionID: sessionID,
		})
	}
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
			if err := s.proxy.Restore(snap); err == nil {
				_ = store.ClearProxySnapshot()
			} else {
				s.appendLog("could not restore the saved system proxy; will retry on next launch")
			}
		}
		recents := toRecentNodes(store.LoadRecents())
		s.mu.Lock()
		s.core.recents = recents
		s.mu.Unlock()
	}
	// Resolve the stable endpoint and generate its sourceable shell helper even
	// while disconnected, so Settings can expose it immediately. Failure stays
	// non-fatal here; Connect/GetProxyInfo surface the actionable error.
	if info, err := s.GetProxyInfo(); err != nil {
		s.appendLog("could not prepare local proxy configuration: " + err.Error())
	} else if info.ShellIntegrationError != nil {
		s.appendLog("could not prepare proxy shell helper: " + *info.ShellIntegrationError)
	}
	if warning := s.localProxyPortWarning(); warning != nil {
		s.appendLog(warning.Error())
	}
	s.stopEmit = make(chan struct{})
	go s.emitLoop()
	s.emitCurrent()
}

func (s *Service) Shutdown(ctx context.Context) {
	// Tear down any live tunnel so the OS proxy is restored on quit. Held under
	// connectMu like Connect/Disconnect so a connect racing app-quit can't slip
	// a new connection in behind the teardown.
	s.connectMu.Lock()
	s.teardownExisting()
	s.connectMu.Unlock()
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
//
// connectMu serializes the whole teardown-then-install so two overlapping
// Connect calls can never both tear down the old connection and then race to
// install, which would orphan a live connection with no way to cancel it.
func (s *Service) Connect(brokerURL, targetCountry, targetRelayID string) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

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
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	s.mu.Lock()
	conn := s.conn
	if conn == nil {
		s.mu.Unlock()
		return nil
	}
	conn.disconnecting = true
	// Check finalized and write DISCONNECTING under the SAME lock the finalizer
	// uses for its terminal write: if the flow already claimed the terminal
	// status, skip; otherwise our transient write is ordered before it, so a
	// self-terminating flow racing this Disconnect can never leave the UI stuck
	// on DISCONNECTING.
	if !conn.finalized {
		s.emitStatusLocked(StatusDisconnecting, keepLabel, keepError)
	}
	s.mu.Unlock()

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

// GetProxyInfo returns the stable loopback endpoint and copyable shell helper
// commands. Sourcing the helper is explicit because a GUI process cannot
// mutate an already-running parent shell's environment.
func (s *Service) GetProxyInfo() (NativeProxyInfo, error) {
	port, err := s.localProxyPort()
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
	if warning := s.localProxyPortWarning(); warning != nil {
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

func (s *Service) localProxyPort() (int, error) {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	if s.proxyPort != 0 {
		return s.proxyPort, nil
	}
	resolution, err := proxyconfig.ResolvePort(s.store)
	if err != nil {
		return 0, err
	}
	s.proxyPort = resolution.Port
	s.proxyPortWarn = resolution.PersistenceWarning
	return s.proxyPort, nil
}

func (s *Service) localProxyPortWarning() error {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	return s.proxyPortWarn
}

// tunnelReadyPollInterval is how often awaitTunnelReady dials the mixed inbound
// while waiting for sing-box to bind it.
const tunnelReadyPollInterval = 25 * time.Millisecond

// awaitTunnelReady blocks until the mixed inbound on 127.0.0.1:port accepts a
// loopback connection (sing-box came up), the process exits (crash — a bad
// config or a bind failure), or config.TunnelReadyTimeout elapses. It returns
// the real start-to-ready duration for tunnel_start_ms. On the ready path it
// does NOT consume runErrCh, so the supervisor still owns the live process's
// exit; on the crash path it marks the candidate reaped.
func (s *Service) awaitTunnelReady(ctx context.Context, res *candidateResult, port int) (int64, error) {
	started := time.Now()
	readyLimit := s.tunnelReadyLimit
	if readyLimit <= 0 {
		readyLimit = config.TunnelReadyTimeout
	}
	deadline := started.Add(readyLimit)
	ticker := time.NewTicker(tunnelReadyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case runErr := <-res.runErrCh:
			res.reaped = true
			if runErr == nil {
				runErr = errors.New("sing-box exited")
			}
			return 0, runErr
		case transportErr := <-res.transportErr:
			if transportErr == nil {
				transportErr = errors.New("WSS access transport stopped")
			}
			return 0, transportErr
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
			if s.tunnelReadyProbe()(ctx, port) == nil {
				return time.Since(started).Milliseconds(), nil
			}
			if time.Now().After(deadline) {
				return 0, errors.New("tunnel did not become ready in time")
			}
		}
	}
}

// loopbackReady dials the mixed inbound once to confirm sing-box is accepting
// connections. The connection is closed immediately; sing-box treats it as a
// client that connected and went away.
func loopbackReady(ctx context.Context, port int) error {
	dialer := net.Dialer{Timeout: config.InternetProbeRequestTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// runConnect is the connect flow — fetch, filter, candidate ladder, promote,
// then mid-session supervision — finalized exactly once on exit. The ladder
// semantics are ported from the mobile OpenRungVpnService.connect /
// connectFirstAvailable (the contract's reference implementation).
func (s *Service) runConnect(ctx context.Context, conn *connection, brokerURL, targetCountry, targetRelayID string) {
	defer close(conn.done)
	// Cancel the connect context on every exit — including a terminal failure,
	// which neither Disconnect nor teardownExisting reaches — so the heartbeat
	// loop goroutine (bound to this ctx) never outlives the session.
	defer conn.cancel()
	stage, err := s.connectFlow(ctx, conn, brokerURL, targetCountry, targetRelayID)
	s.finalizeConn(conn, stage, err)
}

// connectFlow runs the connect phases and returns ("", nil) on a clean end (a
// user disconnect or shutdown, at any phase) or the terminal (stage, error).
func (s *Service) connectFlow(ctx context.Context, conn *connection, brokerURL, targetCountry, targetRelayID string) (string, error) {
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

	port, err := s.localProxyPort()
	if err != nil {
		return "proxy_port", err
	}
	if err := proxyconfig.EnsureAvailable(port); err != nil {
		return "proxy_port", err
	}

	fetch, fetchMS, err := s.fetchCandidates(ctx, conn, brokerURL, targetCountry, targetRelayID)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil
		}
		return "broker_fetch", err
	}

	cands, stage, err := s.candidatesFor(fetch.Response, targetCountry, targetRelayID)
	if err != nil {
		return stage, err
	}
	order := s.rankLadder(ctx, cands, targetRelayID)
	ladder := order.candidates()
	s.mu.Lock()
	conn.candidates = ladder
	conn.brokerURL = fetch.BrokerURL
	s.mu.Unlock()

	// Discovery and ranking can take long enough for another process to claim
	// the bind-and-close checked port. Recheck immediately before the ladder so
	// a local collision is not recorded against every relay candidate.
	if err := ctx.Err(); err != nil {
		return "", nil
	}
	if err := proxyconfig.EnsureAvailable(port); err != nil {
		return "proxy_port", err
	}
	res, err := s.runLadder(ctx, conn, ladder, port)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil
		}
		return "relay_connect", err
	}
	order.annotate(res)
	// The OS proxy is pointed at the tunnel only once a candidate is proven, so
	// a fully failing ladder never blackholes the user's traffic — it falls
	// back to the normal network instead (contract: availability over leak).
	if !s.promote(ctx, conn, res, fetchMS, true) {
		return "", nil // user disconnected as the winner came up
	}

	return s.supervise(ctx, conn, res, port, targetCountry, targetRelayID)
}

// fetchCandidates fetches the relay list, using the full directory page size
// for targeted connects so the target is present (the default page may miss
// it), like the mobile client. Returns the fetch duration for broker_fetch_ms.
func (s *Service) fetchCandidates(ctx context.Context, conn *connection, brokerURL, targetCountry, targetRelayID string) (discovery.Fetch, int64, error) {
	displayURL := strings.TrimSpace(brokerURL)
	if displayURL == "" {
		displayURL = config.DefaultBrokerURL
	}
	s.appendLog(fmt.Sprintf("fetching relays from %s", displayURL))

	limit := config.RelayLimit
	if strings.TrimSpace(targetRelayID) != "" || strings.TrimSpace(targetCountry) != "" {
		limit = config.DirectoryRelayLimit
	}
	started := time.Now()
	fetch, err := s.relayFetcher()(ctx, brokerURL, limit, managerClientID(conn.mgr), s.currentSessionID())
	if err != nil {
		return discovery.Fetch{}, 0, err
	}
	return fetch, time.Since(started).Milliseconds(), nil
}

// candidatesFor turns a broker response into the ordered candidate list for
// this connect's target, logging the same lines the mobile console shows.
func (s *Service) candidatesFor(resp relay.ListResponse, targetCountry, targetRelayID string) ([]relay.Descriptor, string, error) {
	// Distinguish "broker returned nothing" from the narrower no-match cases
	// below, so telemetry can tell them apart.
	if len(resp.Relays) == 0 {
		return nil, "relay_select", client.ErrNoRelaysAvailable
	}
	usable := usableRelays(resp)
	s.appendLog(fmt.Sprintf("broker returned %d relays; %d usable", len(resp.Relays), len(usable)))
	if len(usable) == 0 {
		return nil, "relay_select", client.ErrNoUsableRelay
	}

	cands, stage, err := filterCandidates(usable, targetCountry, targetRelayID)
	if err != nil {
		return nil, stage, err
	}
	switch {
	case strings.TrimSpace(targetRelayID) != "":
		name := strings.TrimSpace(cands[0].Label)
		if name == "" {
			name = cands[0].ID
		}
		s.appendLog(fmt.Sprintf("connecting to relay %s", name))
	case strings.TrimSpace(targetCountry) != "":
		s.appendLog(fmt.Sprintf("connecting to a relay in %s", strings.ToUpper(strings.TrimSpace(targetCountry))))
	}
	return cands, "", nil
}

// runLadder walks the candidates in the order it is given — ladder order, which
// rankLadder decides (see ranker.go); broker order only survives where ranking
// does not apply. Each failed candidate is fully torn down before the next is
// tried: sequential by construction, since the shared loopback port cannot be
// rebound until the previous sing-box is reaped. Mirrors the mobile
// connectFirstAvailable.
func (s *Service) runLadder(ctx context.Context, conn *connection, cands []relay.Descriptor, port int) (*candidateResult, error) {
	conn.wssTicketRetryUsed = false
	var lastErr error
	for i, cand := range cands {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := s.attemptCandidate(ctx, conn, cand, port, i+1)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			// A racing disconnect cancelled the attempt mid-rung; don't blame
			// the relay and don't keep trying.
			return nil, ctx.Err()
		}
		lastErr = err
		if stage, local := localCandidateErrorStage(err); local {
			s.appendLog(fmt.Sprintf("local VPN setup failed at %s: %v", stage, err))
			return nil, fmt.Errorf("local VPN setup failed: %w", err)
		}
		if !relayFailureAlreadyRecorded(err) {
			s.recordRelayAttemptFailed(conn.mgr, cand.ID, err, i+1)
		}
		s.appendLog(fmt.Sprintf("relay %s failed: %v", cand.ID, err))
	}
	// Wrap so lastError shows the mobile all-failed message while telemetry
	// still classifies on the real root cause.
	return nil, fmt.Errorf("All relay connection attempts failed. Last error: %w", lastErr)
}

// attemptCandidate always runs the legacy direct path first. Only a typed raw
// TCP or post-ready data-path failure can unlock this exact relay's signed WSS
// fronts. Local engine/configuration failures stop without requesting a ticket.
func (s *Service) attemptCandidate(ctx context.Context, conn *connection, cand relay.Descriptor, port, attempt int) (*candidateResult, error) {
	directResult, directErr := s.attemptDirectCandidate(ctx, conn, cand, port, attempt)
	if directErr == nil {
		return directResult, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, eligible := directPathErrorStage(directErr); !eligible {
		return nil, directErr
	}
	fronts := supportedWSSFronts(cand)
	if len(fronts) == 0 {
		return nil, directErr
	}

	// The direct path is an independently meaningful relay-health signal. Record
	// it once before transport fallback; subsequent ticket/CDN/WSS failures must
	// not add another relay-health penalty.
	s.recordRelayAttemptFailed(conn.mgr, cand.ID, directErr, attempt)
	s.recordTransportFallback(conn.mgr, cand.ID, directErr)
	s.appendLog(fmt.Sprintf("direct path to relay %s failed; trying its WSS fronts", cand.ID))
	lastErr := directErr
	for _, front := range fronts {
		result, err := s.attemptWSSCandidate(ctx, conn, cand, front, port, attempt)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, local := localCandidateErrorStage(err); local {
			return nil, err
		}
		lastErr = err
		if _, transportFailure := wssTransportStage(err); transportFailure {
			s.recordWSSTransportFailed(conn.mgr, cand.ID, err)
		}
		s.appendLog(fmt.Sprintf("WSS front %s failed: %v", front.ID, err))
	}
	return nil, markRelayFailureRecorded(fmt.Errorf("direct path failed (%v); WSS fallback failed: %w", directErr, lastErr))
}

// attemptDirectCandidate is the existing direct/punched rung split from its
// path-independent sing-box lifecycle so WSS can reuse that lifecycle safely.
func (s *Service) attemptDirectCandidate(ctx context.Context, conn *connection, cand relay.Descriptor, port, attempt int) (*candidateResult, error) {
	s.appendLog(fmt.Sprintf("trying relay %s at %s:%d", cand.ID, cand.PublicHost, cand.PublicPort))
	s.appendLog("checking relay TCP reachability")
	tcpMS, err := s.relayDialer()(ctx, cand.PublicHost, cand.PublicPort)
	if err != nil {
		return nil, markDirectPathError("tcp", err)
	}

	candCtx, cancel := context.WithCancel(ctx)
	res := &candidateResult{
		relay: cand, accessTransport: relay.TransportDirect,
		ctx: candCtx, cancel: cancel, proxyPort: port,
		tcpMS: tcpMS, hasTCPMS: true, attempt: int64(attempt), brokerIndex: -1,
	}

	// Try a direct NAT-punched path first; on any failure fall back to the
	// relay hub endpoint so the outcome is never worse than not punching.
	configInput := client.SingBoxConfigInput{
		Relay:              cand,
		Mode:               client.ModeProxy,
		ProxyListenAddress: proxyconfig.Host,
		ProxyListenPort:    port,
	}
	if est := s.maybePunch(candCtx, conn.mgr, cand); est != nil {
		res.punch = est
		res.accessTransport = "punch"
		configInput.BridgeHost = est.BridgeHost
		configInput.BridgePort = est.BridgePort
		configInput.PunchPeerExcludeAddress = est.PeerIP
		go func() { _ = est.Bridge.Serve(candCtx) }()
		s.appendLog(fmt.Sprintf("punched direct path to %s (peer %s, nat %s)", cand.ID, est.PeerIP, est.NATClass))
	}
	return s.startCandidate(res, configInput)
}

// startCandidate owns path-independent config, process, readiness, and
// end-to-end validation. Every path uses identical inner Reality settings.
func (s *Service) startCandidate(res *candidateResult, configInput client.SingBoxConfigInput) (*candidateResult, error) {
	configJSON, err := client.BuildSingBoxConfig(configInput)
	if err != nil {
		res.teardown()
		return nil, markLocalCandidateError("config", err)
	}
	configPath, err := s.candidateConfigWriter()(configJSON)
	if err != nil {
		res.teardown()
		return nil, markLocalCandidateError("config_file", err)
	}
	res.configPath = configPath

	res.runErrCh = make(chan error, 1)
	go func(errCh chan<- error, path string) { errCh <- s.tunnelRunner()(res.ctx, path) }(res.runErrCh, configPath)

	// Wait until sing-box binds the mixed inbound (a real start measurement, and
	// far faster than a fixed grace when the engine is ready in tens of ms), or
	// it dies first — either way the candidate is decided before the probe.
	startMS, err := s.awaitTunnelReady(res.ctx, res, res.proxyPort)
	if err != nil {
		res.teardown()
		if _, transportFailure := wssTransportStage(err); transportFailure {
			return nil, err
		}
		return nil, markLocalCandidateError("tunnel_start", err)
	}
	res.startMS = startMS

	s.appendLog("verifying internet access through the VPN")
	probeMS, err := s.probeCandidate(res)
	if err != nil {
		res.teardown()
		if _, local := localCandidateErrorStage(err); local {
			return nil, err
		}
		if _, transportFailure := wssTransportStage(err); transportFailure {
			return nil, err
		}
		if res.accessTransport == relay.TransportDirect || res.accessTransport == "punch" {
			return nil, markDirectPathError("internet_probe", err)
		}
		return nil, err
	}
	res.probeMS = probeMS
	s.appendLog(fmt.Sprintf("internet access verified in %d ms", probeMS))
	return res, nil
}

// probeCandidate watches both the process and WSS adapter while the internet
// probe is running. A sing-box exit is local; a WSS session exit is transport-
// scoped; only a completed failing probe is a relay data-path result.
func (s *Service) probeCandidate(res *candidateResult) (int64, error) {
	type probeResult struct {
		ms  int64
		err error
	}
	probeCh := make(chan probeResult, 1)
	go func() {
		ms, err := s.tunnelProber()(res.ctx, res.proxyPort)
		probeCh <- probeResult{ms: ms, err: err}
	}()
	select {
	case result := <-probeCh:
		return result.ms, result.err
	case runErr := <-res.runErrCh:
		res.reaped = true
		if runErr == nil {
			runErr = errors.New("sing-box exited during internet verification")
		}
		return 0, markLocalCandidateError("tunnel_probe_process", runErr)
	case transportErr := <-res.transportErr:
		if transportErr == nil {
			transportErr = markWSSTransportError("wss_session", res.frontID, errors.New("WSS access transport stopped"))
		}
		return 0, transportErr
	case <-res.ctx.Done():
		return 0, res.ctx.Err()
	}
}

// connectMeasurements is the winning candidate's timing, reported on the
// initial connection_succeeded or a recovery relay_failover so the broker's
// relay ranking credits the relay that actually carried the connection.
func connectMeasurements(res *candidateResult, brokerFetchMS int64) map[string]int64 {
	m := map[string]int64{
		"broker_fetch_ms":   brokerFetchMS,
		"tunnel_start_ms":   res.startMS,
		"internet_probe_ms": res.probeMS,
		"relay_attempts":    res.attempt,
		// Rank observability: where the winning relay sat in broker order before
		// ranking, and what the ranker measured for it — the pair that shows
		// whether client-side ranking actually beats broker order on
		// tunnel_start_ms. relay_probe_ms is absent, never zero, when the relay
		// was not probed: 0ms is a legitimate measurement.
		"relay_broker_index": res.brokerIndex,
	}
	if res.hasTCPMS {
		m["relay_tcp_ms"] = res.tcpMS
	}
	if res.accessTransport == accessTransportWSS {
		m["transport_connect_ms"] = res.transportMS
	}
	if res.rankProbeMS != nil {
		m["relay_probe_ms"] = *res.rankProbeMS
	}
	return m
}

// promote adopts a winning candidate as the live tunnel: it marks CONNECTED with
// the broker-served location label (never a raw IP), records recents, points the
// OS proxy at the tunnel, records the initial connection_succeeded when asked,
// and starts the heartbeat loop (once per session). Recovery telemetry is
// recorded by supervise with the transition attributes it owns.
//
// The disconnect guard and the CONNECTED publish happen under one lock, so a
// Disconnect that set disconnecting first is always seen (the connect bails —
// mirroring the mobile ensureActive guard — with no CONNECTED flash and no
// recorded success), and one that arrives after is fully ordered behind the
// publish. Returns false without publishing anything when it bailed.
func (s *Service) promote(ctx context.Context, conn *connection, res *candidateResult, brokerFetchMS int64, initial bool) bool {
	label := geoLabel(res.relay)
	recent := recentFrom(res.relay)
	s.appendLog("connected via " + label)

	s.mu.Lock()
	if conn.disconnecting || ctx.Err() != nil {
		s.mu.Unlock()
		res.teardown()
		return false
	}
	conn.active = res
	conn.activeRelayID = res.relay.ID
	s.markConnectedLocked(label, recent)
	s.mu.Unlock()

	s.applyProxy(conn, res.proxyPort)
	if conn.mgr != nil {
		conn.mgr.MarkConnected(res.relay.ID)
		if initial {
			attrs := map[string]string{"transport": res.accessTransport}
			if res.frontID != "" {
				attrs["front_id"] = res.frontID
			}
			conn.mgr.Record("connection_succeeded", res.relay.ID, attrs, connectMeasurements(res, brokerFetchMS))
			_ = conn.mgr.Flush(ctx)
		}
		conn.heartbeatOnce.Do(func() { go conn.mgr.RunHeartbeatLoop(ctx) })
	}
	return true
}

// recordRelayAttemptFailed dents the failing relay's broker ranking. attempt is
// the 1-based ladder rung; pass 0 for a mid-session failover trigger (not a
// rung, so no attempt measurement).
func (s *Service) recordRelayAttemptFailed(mgr *clienttelemetry.Manager, relayID string, err error, attempt int) {
	if mgr == nil {
		return
	}
	attrs := map[string]string{}
	if reason := clienttelemetry.ClassifyError(err); reason != "" {
		attrs["failure_reason"] = reason
	}
	if detail := clienttelemetry.ErrorDetail(err); detail != "" {
		attrs["failure_detail"] = detail
	}
	var meas map[string]int64
	if attempt > 0 {
		meas = map[string]int64{"attempt": int64(attempt)}
	}
	mgr.Record("relay_attempt_failed", relayID, attrs, meas)
}

// applyProxy points the OS proxy at the local mixed inbound. The pre-tunnel
// setting is snapshotted exactly once per connection (a recovery release keeps
// it, so a re-promote can re-point without capturing our own proxy as the
// user's), persisted for crash recovery, and restored on exit. Failure is
// non-fatal: sing-box still listens on loopback, so the app can fall back to a
// manual proxy address.
func (s *Service) applyProxy(conn *connection, port int) {
	if !s.proxy.Supported() {
		s.appendLog(fmt.Sprintf("system proxy unavailable here; set manual proxy %s:%d", proxyconfig.Host, port))
		return
	}
	if !conn.snapshotTaken {
		snap, err := s.proxy.Snapshot()
		if err != nil {
			s.appendLog("could not read current system proxy; leaving it unchanged")
			return
		}
		conn.snapshot = snap
		conn.snapshotTaken = true
		if s.store != nil {
			_ = s.store.SaveProxySnapshot(snap) // persist for crash recovery
		}
	}
	// Mark restoration pending before Set: platform controllers can mutate OS
	// state and only then fail while notifying applications of the change.
	conn.proxySet = true
	if err := s.proxy.Set(proxyconfig.Host, port); err != nil {
		s.appendLog(fmt.Sprintf("system proxy set failed; set manual proxy %s:%d", proxyconfig.Host, port))
		// A failed Set may have partially applied: put the captured setting back
		// so the user's proxy is never left pointing at us with nothing there.
		if restoreErr := s.proxy.Restore(conn.snapshot); restoreErr != nil {
			s.appendLog("system proxy restore after failed set failed; will retry on next launch")
			return
		}
		conn.proxySet = false
		if s.store != nil {
			_ = s.store.ClearProxySnapshot()
		}
		// Keep snapshotTaken so the true pre-tunnel snapshot is retained: a later
		// re-promote must NOT re-capture (the user may have set a manual proxy at
		// our own suggestion, which we must never treat as their prior state).
		// The successful-Set path below re-persists the retained snapshot.
		return
	}
	// Ensure proxySet=true always implies a persisted snapshot for crash
	// recovery, even if an earlier Set failure cleared it (idempotent for the
	// common first-Set-succeeds path).
	if s.store != nil {
		_ = s.store.SaveProxySnapshot(conn.snapshot)
	}
	s.appendLog(fmt.Sprintf("proxy listening on %s:%d", proxyconfig.Host, port))
}

// releaseProxy points the OS proxy back at the user's captured setting while
// keeping the snapshot, so a mid-session recovery lets traffic fall back to the
// normal network during the reconnect gap and a re-promote can re-point.
func (s *Service) releaseProxy(conn *connection) bool {
	if conn.proxySet {
		if err := s.proxy.Restore(conn.snapshot); err != nil {
			s.appendLog("system proxy restore failed; keeping the recovery snapshot for the next retry")
			return false
		}
		conn.proxySet = false
	}
	return true
}

// cleanupConn tears down the live candidate (sing-box, punched path, temp
// config — in that pinned order), restores the OS proxy, and clears the
// persisted snapshot. Safe to call once per connection on exit.
func (s *Service) cleanupConn(conn *connection) {
	s.mu.Lock()
	active := conn.active
	conn.active = nil
	s.mu.Unlock()
	active.teardown()
	restored := s.releaseProxy(conn)
	if restored && s.store != nil {
		_ = s.store.ClearProxySnapshot()
	}
}

// finalizeConn is the single exit path for a connect flow: it releases the live
// resources and lands the state machine on disconnected (user intent, whatever
// phase it raced — never reported as a failure) or failed (everything else).
func (s *Service) finalizeConn(conn *connection, stage string, err error) {
	// Claim ownership of the terminal status before releasing resources, so a
	// Disconnect racing the teardown skips its own transient DISCONNECTING
	// write instead of leaving it stuck after our terminal status lands.
	s.mu.Lock()
	conn.finalized = true
	s.mu.Unlock()

	s.cleanupConn(conn)

	// Re-sample intent AFTER teardown: a Disconnect that arrived during the
	// ~kill-grace teardown must still land on disconnected, not failed.
	s.mu.Lock()
	disconnecting := conn.disconnecting
	activeRelayID := conn.activeRelayID
	s.mu.Unlock()

	switch {
	case disconnecting, err == nil:
		// err == nil without a disconnect only happens when the app is shutting
		// down mid-flow; report it as the clean stop it is.
		if conn.mgr != nil && activeRelayID != "" {
			conn.mgr.Record("tunnel_stopped", activeRelayID, nil, nil)
		}
		s.appendLog("disconnected")
		s.mu.Lock()
		s.emitStatusLocked(StatusDisconnected, clearLabel, clearError)
		s.mu.Unlock()
		endSession(conn.mgr, "disconnect")
	default:
		msg := err.Error()
		s.appendLog("connect failed: " + msg)
		s.mu.Lock()
		s.emitStatusLocked(StatusFailed, keepLabel, setError(msg))
		s.mu.Unlock()
		if conn.mgr != nil {
			attrs := map[string]string{"failure_stage": stage}
			if reason := clienttelemetry.ClassifyError(err); reason != "" {
				attrs["failure_reason"] = reason
			}
			if detail := clienttelemetry.ErrorDetail(err); detail != "" {
				attrs["failure_detail"] = detail
			}
			conn.mgr.Record("connection_failed", "", attrs, nil)
			conn.mgr.EndSession("connection_failed")
			flushOnShutdown(conn.mgr)
		}
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
	s.emitStatusLocked(status, label, errOp)
	s.mu.Unlock()
}

// emitStatusLocked mutates the core status and emits the snapshot while the
// caller holds s.mu. Terminal (finalizeConn) and transient (Disconnect) status
// writes race across goroutines; emitting under the lock makes the last writer
// also the last to emit, so the frontend never ends on a status a later write
// already superseded. The Emitter only posts a UI event, so holding the lock
// across it is cheap and non-reentrant.
func (s *Service) emitStatusLocked(status ConnectionStatus, label labelOp, errOp errorOp) {
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
	s.emit(s.snapshotLocked())
}

func (s *Service) markConnected(label string, recent *RecentNode) {
	s.appendLog("connected via " + label)
	s.mu.Lock()
	s.markConnectedLocked(label, recent)
	s.mu.Unlock()
}

// markConnectedLocked publishes CONNECTED while the caller holds s.mu, so
// promote can decide-and-publish atomically against a racing Disconnect.
func (s *Service) markConnectedLocked(label string, recent *RecentNode) {
	s.core.status = StatusConnected
	l := label
	s.core.relayLabel = &l
	s.core.lastError = nil
	if recent != nil {
		s.core.recents = persistPrepend(s.store, s.core.recents, *recent)
	}
	s.emit(s.snapshotLocked())
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
			// Emit under the lock so this coalesced log flush is ordered against
			// the status writers (which also emit under s.mu): otherwise a stale
			// snapshot captured here could be delivered after a terminal status
			// write, leaving the UI on a superseded transient status.
			s.emit(s.snapshotLocked())
			s.mu.Unlock()
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
