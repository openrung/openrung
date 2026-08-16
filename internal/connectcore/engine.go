// Package connectcore is the UI-agnostic client connection engine
// (docs/adr/001): the connect state machine, candidate ladder and ranking,
// WSS front fallback, punch attempt, mid-session health monitoring and
// failover, directory cache, and probes. The state machine semantics mirror
// the mobile native bridge contract (openrung-mobile-app
// src/native/types.ts, docs/CONTRACT.md §3) so every client that drives this
// engine behaves like the mobile reference implementation.
//
// Everything platform-specific arrives through the narrow interfaces in
// interfaces.go; the desktop webview adapter lives in desktop/vpnservice, and
// this package must not import a UI toolkit or anything under desktop/.
package connectcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/discovery"
	"openrung/internal/punch"
	"openrung/internal/relay"
)

type Status string

const (
	StatusDisconnected  Status = "disconnected"
	StatusPreparing     Status = "preparing"
	StatusConnecting    Status = "connecting"
	StatusConnected     Status = "connected"
	StatusDisconnecting Status = "disconnecting"
	StatusFailed        Status = "failed"
)

// RecentNode mirrors the contract's RecentNode (openrung-mobile-app
// src/native/types.ts): a recently used exit location.
type RecentNode struct {
	CountryCode string
	Label       string
	Latitude    float64
	Longitude   float64
}

// State is the engine's connection state as delivered to the EventSink. It is
// the contract's NativeVpnState minus the log lines, whose ring buffering and
// coalescing belong to the platform sink.
type State struct {
	Status     Status
	RelayLabel *string
	LastError  *string
	Recents    []RecentNode
}

// clientID resolves the stable per-install identifier. It is a package var so
// tests can stub it; it wraps clienttelemetry.ClientID, which persists to
// os.UserConfigDir()/openrung/client-id with correct per-OS paths.
var clientID = clienttelemetry.ClientID

// PlatformCLI identifies the terminal client (cmd/client) on the engine's
// broker traffic. brokerapi maps only the GUI/mobile platforms to fixed
// identification headers, so CLI requests carry no platform header; the label
// instead rides every telemetry event as a "platform" attribute (see
// newManager), which the broker stores as an ordinary free-form attribute.
const PlatformCLI = brokerapi.Platform("cli")

// coreState is the mutable slice of State the engine owns directly.
type coreState struct {
	status     Status
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
	snapshot      OSProxySnapshot
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

// Engine is the connection engine. The platform hooks (Sink, Persistence,
// OSProxy, Elevation, ResolveProxyPort) and options must be assigned before
// Start or the first Connect; the engine never mutates them.
type Engine struct {
	// Sink receives the engine's typed state and log events. A nil Sink drops
	// them (headless drivers that only poll State).
	Sink EventSink

	// Persistence stores recents and the crash-recovery proxy snapshot. Nil
	// disables persistence.
	Persistence Persistence

	// OSProxy points the OS system proxy at the tunnel while connected. Nil
	// behaves like an unsupported platform.
	OSProxy OSProxy

	// Elevation acquires TUN privileges; unused in proxy mode (see Prepare).
	Elevation Elevation

	// ResolveProxyPort resolves the stable local proxy port; the engine pins
	// the first successful resolution for the process (see LocalProxyPort).
	ResolveProxyPort func() (ProxyPortResolution, error)

	// SingBoxPath overrides the sing-box binary path (defaults to "sing-box"
	// resolved via PATH). Packaging points this at the bundled binary.
	SingBoxPath string

	// Platform identifies this host on every broker request the engine makes
	// (relay-list fetches, WSS session tickets, telemetry). Empty means
	// PlatformDesktop: the desktop app predates the field and its wire
	// behavior must stay unchanged.
	Platform brokerapi.Platform

	// PunchEnabled attempts a direct NAT-punched path to punch-capable
	// relays before falling back to the relay hub's data plane. PunchURL
	// overrides the hub punch coordinator base URL (else the relay's
	// advertised punch_endpoint is used). PunchInsecure skips TLS
	// verification of the hub's self-signed punch coordination endpoint
	// (relay hubs on bare IPs cannot get a CA cert); it defaults to false,
	// so a host opts out of hub TLS verification deliberately — see
	// punchHTTPClient for why that stays safe.
	PunchEnabled  bool
	PunchURL      string
	PunchInsecure bool

	// connectMu serializes the Connect/Disconnect mutation surface. Hosts may
	// dispatch every call on its own goroutine (the desktop webview bridge
	// does), so without this two overlapping Connects could both pass
	// teardownExisting and orphan a live connection whose supervisor keeps a
	// tunnel alive forever. mu still guards the finer-grained fields;
	// connectMu only orders whole connect/disconnect operations.
	connectMu sync.Mutex

	mu        sync.Mutex
	core      coreState
	sessionID string
	conn      *connection

	directory *directoryCache

	// proxyPortMu pins only a successfully resolved endpoint for this process.
	// A transient resolution failure remains retryable on the next call.
	// ResolveProxyPort persists automatic selections across launches.
	proxyPortMu   sync.Mutex
	proxyPort     int
	proxyPortWarn error

	// Test seams (nil means the production implementation). They mirror the
	// platform-hook injection pattern above so ladder tests need no network,
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
	healthTick        time.Duration // 0 means HealthProbeInterval
	networkRetryDelay time.Duration // 0 means networkRecoveryPollInterval
	tunnelReadyLimit  time.Duration // 0 means TunnelReadyTimeout
}

func (s *Engine) candidateConfigWriter() func([]byte) (string, error) {
	if s.writeConfig != nil {
		return s.writeConfig
	}
	return writeTempConfig
}

func (s *Engine) tunnelReadyProbe() func(context.Context, int) error {
	if s.tunnelReady != nil {
		return s.tunnelReady
	}
	return loopbackReady
}

func (s *Engine) tunnelRunner() func(context.Context, string) error {
	if s.runTunnel != nil {
		return s.runTunnel
	}
	return func(ctx context.Context, configPath string) error {
		runner := client.SingBoxRunner{
			Path:      s.SingBoxPath,
			Stdout:    s.logWriter(),
			Stderr:    s.logWriter(),
			KillGrace: LadderKillGrace,
		}
		return runner.Run(ctx, configPath)
	}
}

func (s *Engine) tunnelProber() func(context.Context, int) (int64, error) {
	if s.probeTunnel != nil {
		return s.probeTunnel
	}
	return verifyInternetViaProxy
}

func (s *Engine) healthProber() func(context.Context, int) error {
	if s.healthProbe != nil {
		return s.healthProbe
	}
	return healthSweepViaProxy
}

func (s *Engine) relayDialer() func(context.Context, string, int) (int64, error) {
	if s.dialRelay != nil {
		return s.dialRelay
	}
	return func(ctx context.Context, host string, port int) (int64, error) {
		return RelayTCPReachable(ctx, host, port, RelayTCPTimeout)
	}
}

func (s *Engine) relayFetcher() func(context.Context, string, int, string, string) (discovery.Fetch, error) {
	if s.fetchRelays != nil {
		return s.fetchRelays
	}
	return func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		return discovery.FirstReachable(ctx, brokerapi.BrokerCandidates(brokerURL), discovery.Options{
			Limit:     limit,
			ClientID:  clientID,
			SessionID: sessionID,
			Platform:  s.telemetryPlatform(),
		})
	}
}

// telemetryPlatform resolves the platform identity for the engine's broker
// requests. Empty defaults to PlatformDesktop (see the Platform field).
func (s *Engine) telemetryPlatform() brokerapi.Platform {
	if s.Platform == "" {
		return brokerapi.PlatformDesktop
	}
	return s.Platform
}

// notify hands a typed Notice to the sink when it implements NoticeSink. The
// desktop sink predates notices and keeps receiving only state and log events;
// every notice is also described by a log line, so no host loses information.
func (s *Engine) notify(notice Notice) {
	if sink, ok := s.Sink.(NoticeSink); ok {
		sink.Notice(notice)
	}
}

func New() *Engine {
	return &Engine{
		core:         coreState{status: StatusDisconnected},
		directory:    newDirectoryCache(),
		PunchEnabled: true,
	}
}

// Start runs crash recovery and loads persisted recents. It emits nothing;
// the host reads State() afterwards to publish the initial snapshot, exactly
// like the pre-extraction desktop startup did.
func (s *Engine) Start() {
	if s.Persistence != nil {
		// Crash recovery: a leftover proxy snapshot means a prior session died
		// without restoring the OS proxy. Undo it before doing anything else.
		if snap, ok := s.Persistence.LoadProxySnapshot(); ok {
			if s.OSProxy != nil && s.OSProxy.Restore(snap) == nil {
				_ = s.Persistence.ClearProxySnapshot()
			} else {
				s.appendLog("could not restore the saved system proxy; will retry on next launch")
			}
		}
		recents := s.Persistence.LoadRecents()
		s.mu.Lock()
		s.core.recents = recents
		s.mu.Unlock()
	}
}

// Stop tears down any live tunnel so the OS proxy is restored on quit. Held
// under connectMu like Connect/Disconnect so a connect racing app-quit can't
// slip a new connection in behind the teardown.
func (s *Engine) Stop() {
	s.connectMu.Lock()
	s.teardownExisting()
	s.connectMu.Unlock()
}

// Prepare mirrors the mobile bridge's OS-consent step. Proxy mode needs no OS
// consent; TUN mode will perform the elevation handshake here via the
// Elevation hook.
func (s *Engine) Prepare() (bool, error) {
	return true, nil
}

// Connect starts (or switches) the tunnel. targetRelayID takes precedence over
// targetCountry; empty strings stand in for the contract's nulls. It resolves
// once the start has been dispatched — completion is reported via events.
func (s *Engine) Connect(brokerURL, targetCountry, targetRelayID string) error {
	return s.ConnectTarget(brokerURL, RelayTarget{Country: targetCountry, RelayID: targetRelayID})
}

// ConnectTarget is Connect with the full RelayTarget surface: the TUI and the
// headless CLI driver target by label too (-relay-label may name several
// relays), which the contract-shaped Connect signature cannot express.
//
// connectMu serializes the whole teardown-then-install so two overlapping
// Connect calls can never both tear down the old connection and then race to
// install, which would orphan a live connection with no way to cancel it.
func (s *Engine) ConnectTarget(brokerURL string, target RelayTarget) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	s.teardownExisting()

	ctx, cancel := context.WithCancel(context.Background())
	conn := &connection{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	s.setStatus(StatusPreparing, keepLabel, clearError)
	go s.runConnect(ctx, conn, brokerURL, target)
	return nil
}

func (s *Engine) Disconnect() error {
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

// State returns the current engine state.
func (s *Engine) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

// ConnectionInfo describes the live tunnel's access path for host status UIs:
// which relay carries the session, over which access transport, and where the
// local mixed inbound listens. State stays the contract's NativeVpnState shape;
// this is the host-facing detail view beside it.
type ConnectionInfo struct {
	Relay     relay.Descriptor
	Transport string // relay.TransportDirect, "punch", or "wss"
	FrontID   string // set only on the relay-local WSS fallback path
	ProxyPort int
}

// ActiveConnectionInfo returns the promoted candidate's path details, or false
// while no candidate is live (idle, mid-ladder, or a recovery gap).
func (s *Engine) ActiveConnectionInfo() (ConnectionInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.conn.active == nil {
		return ConnectionInfo{}, false
	}
	active := s.conn.active
	return ConnectionInfo{
		Relay:     active.relay,
		Transport: active.accessTransport,
		FrontID:   active.frontID,
		ProxyPort: active.proxyPort,
	}, true
}

// SessionID returns the live telemetry session id, or "" when idle.
func (s *Engine) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// tunnelReadyPollInterval is how often awaitTunnelReady dials the mixed inbound
// while waiting for sing-box to bind it.
const tunnelReadyPollInterval = 25 * time.Millisecond

// awaitTunnelReady blocks until the mixed inbound on 127.0.0.1:port accepts a
// loopback connection (sing-box came up), the process exits (crash — a bad
// config or a bind failure), or TunnelReadyTimeout elapses. It returns
// the real start-to-ready duration for tunnel_start_ms. On the ready path it
// does NOT consume runErrCh, so the supervisor still owns the live process's
// exit; on the crash path it marks the candidate reaped.
func (s *Engine) awaitTunnelReady(ctx context.Context, res *candidateResult, port int) (int64, error) {
	started := time.Now()
	readyLimit := s.tunnelReadyLimit
	if readyLimit <= 0 {
		readyLimit = TunnelReadyTimeout
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
	dialer := net.Dialer{Timeout: InternetProbeRequestTimeout}
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
func (s *Engine) runConnect(ctx context.Context, conn *connection, brokerURL string, target RelayTarget) {
	defer close(conn.done)
	// Cancel the connect context on every exit — including a terminal failure,
	// which neither Disconnect nor teardownExisting reaches — so the heartbeat
	// loop goroutine (bound to this ctx) never outlives the session.
	defer conn.cancel()
	stage, err := s.connectFlow(ctx, conn, brokerURL, target)
	s.finalizeConn(conn, stage, err)
}

// connectFlow runs the connect phases and returns ("", nil) on a clean end (a
// user disconnect or shutdown, at any phase) or the terminal (stage, error).
func (s *Engine) connectFlow(ctx context.Context, conn *connection, brokerURL string, target RelayTarget) (string, error) {
	s.setStatus(StatusConnecting, keepLabel, clearError)

	mgr := s.newManager(brokerURL)
	conn.mgr = mgr
	if mgr != nil {
		if session, err := mgr.BeginSession(); err == nil && session != nil {
			s.mu.Lock()
			s.sessionID = session.ID
			s.mu.Unlock()
		}
		mgr.Record("connection_attempted", "", nil, nil)
	}

	port, err := s.LocalProxyPort()
	if err != nil {
		return "proxy_port", err
	}
	if err := EnsureProxyPortAvailable(port); err != nil {
		return "proxy_port", err
	}

	fetch, fetchMS, err := s.fetchCandidates(ctx, conn, brokerURL, target)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil
		}
		return "broker_fetch", err
	}

	cands, stage, err := s.candidatesFor(fetch.Response, target)
	if err != nil {
		return stage, err
	}
	order := s.rankLadder(ctx, cands, target)
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
	if err := EnsureProxyPortAvailable(port); err != nil {
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

	return s.supervise(ctx, conn, res, port, target)
}

// fetchCandidates fetches the relay list, using the full directory page size
// for targeted connects so the target is present (the default page may miss
// it), like the mobile client. Returns the fetch duration for broker_fetch_ms.
func (s *Engine) fetchCandidates(ctx context.Context, conn *connection, brokerURL string, target RelayTarget) (discovery.Fetch, int64, error) {
	displayURL := strings.TrimSpace(brokerURL)
	if displayURL == "" {
		displayURL = DefaultBrokerURL
	}
	s.appendLog(fmt.Sprintf("fetching relays from %s", displayURL))

	limit := RelayLimit
	if target.Targeted() {
		limit = DirectoryRelayLimit
	}
	started := time.Now()
	fetch, err := s.relayFetcher()(ctx, brokerURL, limit, managerClientID(conn.mgr), s.SessionID())
	if err != nil {
		return discovery.Fetch{}, 0, err
	}
	return fetch, time.Since(started).Milliseconds(), nil
}

// candidatesFor turns a broker response into the ordered candidate list for
// this connect's target, logging the same lines the mobile console shows.
func (s *Engine) candidatesFor(resp relay.ListResponse, target RelayTarget) ([]relay.Descriptor, string, error) {
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

	cands, stage, err := FilterCandidates(usable, target)
	if err != nil {
		return nil, stage, err
	}
	switch {
	case target.identity():
		name := strings.TrimSpace(cands[0].Label)
		if name == "" {
			name = cands[0].ID
		}
		s.appendLog(fmt.Sprintf("connecting to relay %s", name))
	case strings.TrimSpace(target.Country) != "":
		s.appendLog(fmt.Sprintf("connecting to a relay in %s", strings.ToUpper(strings.TrimSpace(target.Country))))
	}
	return cands, "", nil
}

// runLadder walks the candidates in the order it is given — ladder order, which
// rankLadder decides (see ranker.go); broker order only survives where ranking
// does not apply. Each failed candidate is fully torn down before the next is
// tried: sequential by construction, since the shared loopback port cannot be
// rebound until the previous sing-box is reaped. Mirrors the mobile
// connectFirstAvailable.
func (s *Engine) runLadder(ctx context.Context, conn *connection, cands []relay.Descriptor, port int) (*candidateResult, error) {
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
func (s *Engine) attemptCandidate(ctx context.Context, conn *connection, cand relay.Descriptor, port, attempt int) (*candidateResult, error) {
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
		s.notify(Notice{
			Kind:    NoticeWSSFallback,
			RelayID: cand.ID,
			FrontID: front.ID,
			Reason:  directErr.Error(),
		})
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
func (s *Engine) attemptDirectCandidate(ctx context.Context, conn *connection, cand relay.Descriptor, port, attempt int) (*candidateResult, error) {
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
		ProxyListenAddress: ProxyHost,
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
func (s *Engine) startCandidate(res *candidateResult, configInput client.SingBoxConfigInput) (*candidateResult, error) {
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
		if res.accessTransport == accessTransportWSS {
			return nil, markWSSTransportError("wss_internet_probe", res.frontID, err)
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
func (s *Engine) probeCandidate(res *candidateResult) (int64, error) {
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
func (s *Engine) promote(ctx context.Context, conn *connection, res *candidateResult, brokerFetchMS int64, initial bool) bool {
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
func (s *Engine) recordRelayAttemptFailed(mgr *clienttelemetry.Manager, relayID string, err error, attempt int) {
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
func (s *Engine) applyProxy(conn *connection, port int) {
	if s.OSProxy == nil || !s.OSProxy.Supported() {
		s.appendLog(fmt.Sprintf("system proxy unavailable here; set manual proxy %s:%d", ProxyHost, port))
		return
	}
	if !conn.snapshotTaken {
		snap, err := s.OSProxy.Snapshot()
		if err != nil {
			s.appendLog("could not read current system proxy; leaving it unchanged")
			return
		}
		conn.snapshot = snap
		conn.snapshotTaken = true
		if s.Persistence != nil {
			_ = s.Persistence.SaveProxySnapshot(snap) // persist for crash recovery
		}
	}
	// Mark restoration pending before Set: platform controllers can mutate OS
	// state and only then fail while notifying applications of the change.
	conn.proxySet = true
	if err := s.OSProxy.Set(ProxyHost, port); err != nil {
		s.appendLog(fmt.Sprintf("system proxy set failed; set manual proxy %s:%d", ProxyHost, port))
		// A failed Set may have partially applied: put the captured setting back
		// so the user's proxy is never left pointing at us with nothing there.
		if restoreErr := s.OSProxy.Restore(conn.snapshot); restoreErr != nil {
			s.appendLog("system proxy restore after failed set failed; will retry on next launch")
			return
		}
		conn.proxySet = false
		if s.Persistence != nil {
			_ = s.Persistence.ClearProxySnapshot()
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
	if s.Persistence != nil {
		_ = s.Persistence.SaveProxySnapshot(conn.snapshot)
	}
	s.appendLog(fmt.Sprintf("proxy listening on %s:%d", ProxyHost, port))
}

// releaseProxy points the OS proxy back at the user's captured setting while
// keeping the snapshot, so a mid-session recovery lets traffic fall back to the
// normal network during the reconnect gap and a re-promote can re-point.
func (s *Engine) releaseProxy(conn *connection) bool {
	if conn.proxySet {
		if s.OSProxy == nil || s.OSProxy.Restore(conn.snapshot) != nil {
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
func (s *Engine) cleanupConn(conn *connection) {
	s.mu.Lock()
	active := conn.active
	conn.active = nil
	s.mu.Unlock()
	active.teardown()
	restored := s.releaseProxy(conn)
	if restored && s.Persistence != nil {
		_ = s.Persistence.ClearProxySnapshot()
	}
}

// finalizeConn is the single exit path for a connect flow: it releases the live
// resources and lands the state machine on disconnected (user intent, whatever
// phase it raced — never reported as a failure) or failed (everything else).
func (s *Engine) finalizeConn(conn *connection, stage string, err error) {
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
			_ = FlushOnShutdown(conn.mgr)
		}
	}
	s.clearConn(conn)
}

// teardownExisting cancels any active connection and waits for its supervisor to
// finish cleanup, so a switch or shutdown never races two connections.
func (s *Engine) teardownExisting() {
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

func (s *Engine) clearConn(conn *connection) {
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

func (s *Engine) setStatus(status Status, label labelOp, errOp errorOp) {
	s.mu.Lock()
	s.emitStatusLocked(status, label, errOp)
	s.mu.Unlock()
}

// emitStatusLocked mutates the core status and emits the state while the
// caller holds s.mu. Terminal (finalizeConn) and transient (Disconnect) status
// writes race across goroutines; emitting under the lock makes the last writer
// also the last to emit, so the sink never ends on a status a later write
// already superseded. The sink only posts a UI event, so holding the lock
// across it is cheap and non-reentrant.
func (s *Engine) emitStatusLocked(status Status, label labelOp, errOp errorOp) {
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
	s.emitLocked()
}

func (s *Engine) markConnected(label string, recent *RecentNode) {
	s.appendLog("connected via " + label)
	s.mu.Lock()
	s.markConnectedLocked(label, recent)
	s.mu.Unlock()
}

// markConnectedLocked publishes CONNECTED while the caller holds s.mu, so
// promote can decide-and-publish atomically against a racing Disconnect.
func (s *Engine) markConnectedLocked(label string, recent *RecentNode) {
	s.core.status = StatusConnected
	l := label
	s.core.relayLabel = &l
	s.core.lastError = nil
	if recent != nil {
		s.core.recents = s.persistPrepend(s.core.recents, *recent)
	}
	s.emitLocked()
}

func (s *Engine) appendLog(line string) {
	if s.Sink != nil {
		s.Sink.Log(LogEntry{Time: time.Now(), Line: line})
	}
}

func (s *Engine) stateLocked() State {
	return State{
		Status:     s.core.status,
		RelayLabel: s.core.relayLabel,
		LastError:  s.core.lastError,
		Recents:    append([]RecentNode{}, s.core.recents...),
	}
}

func (s *Engine) emitLocked() {
	if s.Sink != nil {
		s.Sink.StateChanged(s.stateLocked())
	}
}

// logWriter adapts the engine's log events to an io.Writer for SingBoxRunner.
type logWriter struct{ s *Engine }

func (s *Engine) logWriter() *logWriter { return &logWriter{s: s} }

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			w.s.appendLog(line)
		}
	}
	return len(p), nil
}
