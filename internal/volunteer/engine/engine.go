// Package engine runs a relay as an embeddable, supervised service:
// Start/Stop lifecycle, status/identity callbacks, and live traffic counters
// instead of cmd/volunteer's one-shot signal-driven flow. GUI apps (the
// OpenRung Volunteer desktop app) bind it to their bridge layer; the orchestration
// mirrors cmd/volunteer/main.go run/runTunnelMode.
package engine

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openrung/internal/relay"
	"openrung/internal/tunnel"
	"openrung/internal/volunteer"
)

// Phase is the engine's lifecycle state, shaped for direct display in a UI.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseStarting    Phase = "starting"
	PhaseProbing     Phase = "probing"
	PhaseRegistering Phase = "registering"
	PhaseOnline      Phase = "online"
	PhaseRetrying    Phase = "retrying"
	PhaseStopping    Phase = "stopping"
)

const (
	ModeAuto   = "auto"
	ModeDirect = "direct"
	ModeTunnel = "tunnel"
)

// Identity is the relay's cryptographic identity. Any empty field is generated
// on first start and reported through Events.OnIdentity so the caller can
// persist it (a stable identity keeps the advertised keys consistent across
// restarts).
type Identity struct {
	ClientID          string `json:"clientId"`
	RealityPrivateKey string `json:"realityPrivateKey"`
	RealityPublicKey  string `json:"realityPublicKey"`
	ShortID           string `json:"shortId"`
}

func (id Identity) complete() bool {
	return id.ClientID != "" && id.RealityPrivateKey != "" && id.RealityPublicKey != "" && id.ShortID != ""
}

// Config describes one relay. Zero values take the same defaults as
// cmd/volunteer where one exists.
type Config struct {
	// BrokerURL is required for direct and auto modes (direct registration).
	BrokerURL string
	// Token optionally authenticates registration and hub attachment.
	Token string
	// Label is the public relay name; generated (adjective-noun) when empty.
	Label string
	// XrayPath locates the xray binary. Defaults to "xray" via PATH.
	XrayPath string
	// ListenPort is the direct-mode public and listen port. Defaults to 443.
	ListenPort int
	// Mode is auto, direct, or tunnel. Auto and tunnel require HubAddr; auto
	// without a hub degrades to direct. Defaults to auto.
	Mode string
	// HubAddr is the relay hub control address (host:port).
	HubAddr string
	// HubHTTPURL overrides the hub HTTP API base URL for reachability probing.
	HubHTTPURL string
	// HubCertFingerprint, when set, pins the hub's TLS leaf certificate to this
	// SHA-256 (hex, colons/case ignored). Relay hubs on bare IPs self-sign, so
	// standard CA verification cannot work; pinning the exact certificate is
	// MITM-proof without a CA. This is the production-safe way to trust a
	// self-signed hub — preferred over HubInsecure, which is test-only.
	HubCertFingerprint string
	// HubInsecure skips hub TLS verification entirely (tests against
	// self-signed hubs). Never set from the GUI; prefer HubCertFingerprint.
	HubInsecure bool
	// HubPlaintext dials the hub without TLS (in-process tests only).
	HubPlaintext bool
	// ServerName and RealityDest configure the Reality camouflage target.
	ServerName  string
	RealityDest string
	// MaxSessions and MaxMbps are advertised capacity hints (not enforced).
	MaxSessions int
	MaxMbps     int
	// HeartbeatInterval is the direct-mode broker heartbeat cadence (min 5s).
	HeartbeatInterval time.Duration
	// Identity seeds the relay identity; missing parts are generated.
	Identity Identity
	// ConfigDir is where the generated xray config (which contains the Reality
	// private key) is written, 0600. Defaults to os.TempDir().
	ConfigDir string
	// Version is reported to the broker/hub as the relay runtime version.
	Version string
	// PunchCapable offers NAT hole punching in tunnel mode.
	PunchCapable bool
	// DisableXray skips launching xray (tests).
	DisableXray bool
	// HTTPClient overrides the broker/probe HTTP client (tests).
	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.XrayPath == "" {
		c.XrayPath = "xray"
	}
	if c.ListenPort == 0 {
		c.ListenPort = 443
	}
	if c.ServerName == "" {
		c.ServerName = "www.cloudflare.com"
	}
	if c.RealityDest == "" {
		c.RealityDest = "www.cloudflare.com:443"
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 8
	}
	if c.MaxMbps == 0 {
		c.MaxMbps = 20
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.ConfigDir == "" {
		c.ConfigDir = os.TempDir()
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	return c
}

func (c Config) validate() error {
	switch c.Mode {
	case ModeAuto, ModeDirect, ModeTunnel:
	default:
		return fmt.Errorf("mode must be auto, direct, or tunnel")
	}
	if c.Mode == ModeTunnel && c.HubAddr == "" {
		return fmt.Errorf("hub address is required in tunnel mode")
	}
	if c.Mode != ModeTunnel && c.BrokerURL == "" {
		return fmt.Errorf("broker URL is required")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("listen port must be between 1 and 65535")
	}
	if c.MaxSessions < 1 {
		return fmt.Errorf("max sessions must be at least 1")
	}
	if c.MaxMbps < 1 {
		return fmt.Errorf("max Mbps must be at least 1")
	}
	if c.HeartbeatInterval < 5*time.Second {
		return fmt.Errorf("heartbeat interval must be at least 5s")
	}
	return nil
}

// Status is a UI-ready snapshot of the engine.
type Status struct {
	Phase     Phase  `json:"phase"`
	Transport string `json:"transport,omitempty"`
	RelayID   string `json:"relayId,omitempty"`
	Label     string `json:"label,omitempty"`
	// PublicHost/PublicPort are the endpoint clients see (the hub's in tunnel
	// mode, the relay host's in direct mode).
	PublicHost string `json:"publicHost,omitempty"`
	PublicPort int    `json:"publicPort,omitempty"`
	LastError  string `json:"lastError,omitempty"`
	// StartedAtMs is when the engine went online (unix ms), 0 when not online.
	StartedAtMs       int64  `json:"startedAtMs"`
	ActiveConnections int64  `json:"activeConnections"`
	TotalConnections  uint64 `json:"totalConnections"`
	// BytesFromClients/BytesToClients count relayed traffic in both directions
	// across the engine's lifetime (survives supervised restarts).
	BytesFromClients uint64 `json:"bytesFromClients"`
	BytesToClients   uint64 `json:"bytesToClients"`
}

// Events are the engine's callbacks. All are optional and must not block; they
// are invoked from engine goroutines.
type Events struct {
	// OnStatus fires on every phase/registration transition (not on counter
	// changes — poll Status() for live traffic numbers).
	OnStatus func(Status)
	// OnIdentity fires when the relay identity materializes (first generation
	// or confirmation of the seeded one) so the caller can persist it.
	OnIdentity func(Identity)
	// Log receives engine and xray process output lines.
	Log io.Writer
}

// Engine supervises one relay. Use New, then Start/Stop. Safe for
// concurrent use.
type Engine struct {
	mu     sync.Mutex
	cfg    Config
	events Events

	cancel context.CancelFunc
	done   chan struct{}

	phase      Phase
	transport  string
	relayID    string
	label      string
	publicHost string
	publicPort int
	lastErr    string
	onlineAt   time.Time

	// Direct-mode counters (observer events). Tunnel-mode counters live in
	// tunnelStats; Status() sums both (only one transport is active at a time).
	active      atomic.Int64
	total       atomic.Uint64
	bytesFrom   atomic.Uint64
	bytesTo     atomic.Uint64
	tunnelStats tunnel.TrafficStats
}

func New(cfg Config, events Events) *Engine {
	if events.Log == nil {
		events.Log = io.Discard
	}
	return &Engine{cfg: cfg.withDefaults(), events: events, phase: PhaseIdle}
}

// Start launches the supervised relay loop. It returns a config validation
// error immediately; runtime failures surface through status (PhaseRetrying
// with LastError). Starting a running engine is a no-op.
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.cancel != nil {
		e.mu.Unlock()
		return nil
	}
	if err := e.cfg.validate(); err != nil {
		e.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
	e.mu.Unlock()

	go e.run(ctx, done)
	return nil
}

// Stop cancels the relay and blocks until the supervisor exits (xray reaped,
// listeners closed). Stopping an idle engine is a no-op.
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	e.setStatus(func() { e.phase = PhaseStopping })
	cancel()
	<-done
}

// Running reports whether Start has been called and Stop has not completed.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancel != nil
}

// UpdateConfig replaces the engine configuration. It only applies while
// stopped; calling it on a running engine returns an error.
func (e *Engine) UpdateConfig(cfg Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		return errors.New("cannot change configuration while the relay is running")
	}
	e.cfg = cfg.withDefaults()
	return nil
}

// Status returns a point-in-time snapshot including live traffic counters.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statusLocked()
}

func (e *Engine) statusLocked() Status {
	ts := e.tunnelStats.Snapshot()
	var startedMs int64
	if !e.onlineAt.IsZero() {
		startedMs = e.onlineAt.UnixMilli()
	}
	return Status{
		Phase:             e.phase,
		Transport:         e.transport,
		RelayID:           e.relayID,
		Label:             e.label,
		PublicHost:        e.publicHost,
		PublicPort:        e.publicPort,
		LastError:         e.lastErr,
		StartedAtMs:       startedMs,
		ActiveConnections: e.active.Load() + ts.ActiveStreams,
		TotalConnections:  e.total.Load() + ts.TotalStreams,
		BytesFromClients:  e.bytesFrom.Load() + ts.BytesFromClients,
		BytesToClients:    e.bytesTo.Load() + ts.BytesToClients,
	}
}

// setStatus applies a mutation under the lock and emits the fresh snapshot.
func (e *Engine) setStatus(mutate func()) {
	e.mu.Lock()
	mutate()
	snap := e.statusLocked()
	onStatus := e.events.OnStatus
	e.mu.Unlock()
	if onStatus != nil {
		onStatus(snap)
	}
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.events.Log, format+"\n", args...)
}

// run is the supervisor: it re-runs sessions with capped exponential backoff
// until the context is cancelled, so transient failures (broker outage, xray
// crash, network change) self-heal without user action.
func (e *Engine) run(ctx context.Context, done chan struct{}) {
	defer func() {
		e.setStatus(func() {
			e.phase = PhaseIdle
			e.transport = ""
			e.relayID = ""
			e.publicHost = ""
			e.publicPort = 0
			e.onlineAt = time.Time{}
		})
		e.mu.Lock()
		e.cancel = nil
		e.done = nil
		e.mu.Unlock()
		close(done)
	}()

	const backoffMin, backoffMax = 2 * time.Second, time.Minute
	brokerConfig := e.currentConfig()
	broker := &volunteer.BrokerClient{
		BaseURL:    brokerConfig.BrokerURL,
		Token:      brokerConfig.Token,
		HTTPClient: brokerConfig.HTTPClient,
	}
	backoff := backoffMin
	for {
		sessionStart := time.Now()
		err := e.runSession(ctx, broker)
		if ctx.Err() != nil {
			return
		}
		// An intentional restart (auto mode picked a better transport, or the
		// public IP moved) is not a failure: restart promptly with fresh backoff
		// and no error surfaced to the UI.
		if errors.Is(err, errReresolve) || errors.Is(err, errPublicIPChanged) {
			backoff = backoffMin
			e.logf("restarting: %v", err)
			e.setStatus(func() {
				e.phase = PhaseStarting
				e.lastErr = ""
			})
			continue
		}
		if err == nil {
			err = errors.New("relay session ended unexpectedly")
		}
		// A session that survived a while earns a fresh backoff: this failure
		// is a new outage, not the same one continuing.
		if time.Since(sessionStart) > 2*time.Minute {
			backoff = backoffMin
		}
		e.logf("relay stopped: %v — retrying in %s", err, backoff)
		e.setStatus(func() {
			e.phase = PhaseRetrying
			e.lastErr = err.Error()
			e.onlineAt = time.Time{}
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// errPublicIPChanged restarts the session so registration re-runs with the
// relay's new address.
var errPublicIPChanged = errors.New("public address changed")

// errReresolve restarts the session because auto mode re-evaluated reachability
// and now prefers a different transport — e.g. a relay host that fell back to the
// hub (or started while the hub was down) is now confirmed directly reachable,
// so it should stop tunnelling and serve directly.
var errReresolve = errors.New("auto mode re-resolved to a different transport")

// detectPublicIPv6 is a package var so tests can stub public-IPv6 detection. It
// is used for the direct-session public-address checks; auto mode never guesses
// direct from it (see autoResolve).
var detectPublicIPv6 = volunteer.DefaultPublicIPv6Address

// autoReprobeInterval is how often auto mode re-evaluates reachability while a
// session runs, so it converges to the right transport after a change (hub
// outage/recovery, a port opening). A var so tests can shorten it.
var autoReprobeInterval = 3 * time.Minute

// autoResolve decides the transport for auto mode. Direct mode is chosen ONLY
// when a probe positively confirms this relay host is reachable from the internet
// — never speculatively. A probe error means the hub's HTTP API is unreachable,
// so we cannot verify reachability; guessing "direct" there would advertise a
// possibly-firewalled address (a public IPv6 does not imply inbound reachability
// through a router/OS firewall) and, if wrong, leave a dead relay in the
// directory. Since tunnel mode also needs the hub, there is nothing to gain by
// guessing: we tunnel-and-retry, and the periodic re-probe (watchForModeChange)
// promotes to direct the instant the hub confirms reachability. Both a probe
// error and a definitive "not reachable" therefore map to tunnel. Returns
// ("","") when ctx is cancelled.
func (e *Engine) autoResolve(ctx context.Context, cfg Config) (mode, publicHost string) {
	hubHTTP := volunteer.DeriveHubHTTPBase(cfg.HubHTTPURL, cfg.HubAddr, !cfg.HubPlaintext)
	reachable, observed, err := volunteer.DetectDirectReachable(ctx, hubHTTP, cfg.Token, "::", cfg.ListenPort, e.probeClient(cfg))
	if ctx.Err() != nil {
		return "", ""
	}
	if err == nil && reachable {
		return ModeDirect, observed
	}
	return ModeTunnel, ""
}

// watchForModeChange runs only in auto mode: it periodically re-resolves the
// reachability decision and signals on ch when the preferred transport differs
// from current, so the supervisor restarts in the better mode. This is what lets
// a directly reachable relay recover to direct after a hub outage instead of
// tunnelling forever, and switch back when reachability changes.
func (e *Engine) watchForModeChange(ctx context.Context, cfg Config, current string, ch chan<- struct{}) {
	ticker := time.NewTicker(autoReprobeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m, _ := e.autoResolve(ctx, cfg)
			if ctx.Err() != nil || m == "" || m == current {
				continue
			}
			select {
			case ch <- struct{}{}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func (e *Engine) runSession(ctx context.Context, broker *volunteer.BrokerClient) error {
	cfg := e.currentConfig()

	e.setStatus(func() {
		e.phase = PhaseStarting
		e.lastErr = ""
	})

	label := cfg.Label
	if label == "" {
		label = volunteer.GenerateLabel()
	} else {
		normalized, err := relay.NormalizeLabel(label)
		if err != nil {
			return fmt.Errorf("invalid label: %w", err)
		}
		label = normalized
	}
	e.setStatus(func() { e.label = label })

	identity, err := e.prepareIdentity(cfg)
	if err != nil {
		return err
	}

	mode := cfg.Mode
	publicHost := ""
	if mode == ModeAuto {
		if cfg.HubAddr == "" {
			// No hub configured: direct is the only option.
			mode = ModeDirect
		} else {
			e.setStatus(func() { e.phase = PhaseProbing })
			mode, publicHost = e.autoResolve(ctx, cfg)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if mode == ModeDirect {
				e.logf("directly reachable at %s — using direct mode", net.JoinHostPort(publicHost, strconv.Itoa(cfg.ListenPort)))
			} else {
				e.logf("not directly reachable, or the relay hub is unavailable — using tunnel mode via the relay hub")
			}
		}
	}

	if mode == ModeTunnel {
		return e.runTunnelSession(ctx, cfg, label, identity)
	}
	return e.runDirectSession(ctx, broker, cfg, label, identity, publicHost)
}

func (e *Engine) currentConfig() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// prepareIdentity fills any missing identity parts (generating Reality keys via
// `xray x25519`), reports the result once, and caches it back into the config
// so later sessions reuse it even if the caller does not persist it.
func (e *Engine) prepareIdentity(cfg Config) (Identity, error) {
	id := cfg.Identity
	generated := false

	if id.ClientID == "" {
		v, err := volunteer.GenerateUUID()
		if err != nil {
			return Identity{}, fmt.Errorf("generate client ID: %w", err)
		}
		id.ClientID = v
		generated = true
	}
	if id.ShortID == "" {
		v, err := volunteer.GenerateShortID()
		if err != nil {
			return Identity{}, fmt.Errorf("generate short ID: %w", err)
		}
		id.ShortID = v
		generated = true
	}
	if id.RealityPrivateKey == "" || id.RealityPublicKey == "" {
		keyPair, err := volunteer.GenerateRealityKeyPair(cfg.XrayPath)
		if err != nil {
			return Identity{}, err
		}
		id.RealityPrivateKey = keyPair.PrivateKey
		id.RealityPublicKey = keyPair.PublicKey
		generated = true
	}

	if generated {
		e.mu.Lock()
		e.cfg.Identity = id
		e.mu.Unlock()
		if e.events.OnIdentity != nil {
			e.events.OnIdentity(id)
		}
	}
	return id, nil
}

func (e *Engine) probeClient(cfg Config) *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	// The reachability probe hits the hub's HTTPS API, which for a self-signed
	// hub presents the same leaf the tunnel pins. It must apply the SAME trust
	// policy as the tunnel dial (hubTLSClientConfig) — otherwise the probe fails
	// certificate validation, auto mode always falls back to tunnel, and a
	// publicly reachable relay never selects direct mode.
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: hubTLSClientConfig(cfg)},
	}
}

// startXray writes the generated config (0600) and launches xray bound to
// listenHost:listenPort, returning the process handle and its exit channel.
func (e *Engine) startXray(ctx context.Context, cfg Config, identity Identity, listenHost string, listenPort int) (*exec.Cmd, <-chan error, error) {
	xrayConfig, err := volunteer.BuildXrayConfig(volunteer.XrayConfigInput{
		ListenHost:        listenHost,
		ListenPort:        listenPort,
		ClientID:          identity.ClientID,
		Flow:              relay.FlowVision,
		Dest:              cfg.RealityDest,
		ServerName:        cfg.ServerName,
		RealityPrivateKey: identity.RealityPrivateKey,
		ShortID:           identity.ShortID,
	})
	if err != nil {
		return nil, nil, err
	}

	configPath := filepath.Join(cfg.ConfigDir, "openrung-volunteer-xray.json")
	if err := os.WriteFile(configPath, xrayConfig, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write xray config: %w", err)
	}

	if cfg.DisableXray {
		return nil, make(chan error), nil
	}

	cmd := exec.CommandContext(ctx, cfg.XrayPath, "run", "-config", configPath)
	volunteer.ConfigureBackgroundCommand(cmd)
	logw := e.events.Log
	cmd.Stdout = logw
	cmd.Stderr = logw
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start xray: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	e.logf("started xray (pid %d)", cmd.Process.Pid)
	return cmd, waitCh, nil
}

func (e *Engine) runDirectSession(ctx context.Context, broker *volunteer.BrokerClient, cfg Config, label string, identity Identity, publicHost string) error {
	if publicHost == "" {
		detected, err := detectPublicIPv6()
		if err != nil {
			return fmt.Errorf("this computer has no public address the internet can reach (no global IPv6): %w", err)
		}
		publicHost = detected
	}

	// The observer owns the public port and forwards to xray on loopback; it is
	// also the traffic-counter source.
	targetHost, targetPort, err := volunteer.ReserveLoopbackTCPPort()
	if err != nil {
		return err
	}

	xrayCmd, xrayErr, err := e.startXray(ctx, cfg, identity, targetHost, targetPort)
	if err != nil {
		return err
	}
	defer stopProcess(xrayCmd, xrayErr)

	observer := &volunteer.ConnectionObserver{
		ListenHost: "::",
		ListenPort: cfg.ListenPort,
		TargetHost: targetHost,
		TargetPort: targetPort,
		// Per-connection lines include client IPs; the engine keeps them out of
		// the UI log and surfaces aggregate counters instead.
		Output: io.Discard,
		Events: &volunteer.ConnectionEvents{
			Opened: func(uint64, string) {
				e.active.Add(1)
				e.total.Add(1)
			},
			Closed: func(_ uint64, _ string, _ time.Duration, fromClient, toClient int64) {
				e.active.Add(-1)
				e.bytesFrom.Add(uint64(fromClient))
				e.bytesTo.Add(uint64(toClient))
			},
		},
	}
	observerCtx, cancelObserver := context.WithCancel(ctx)
	defer cancelObserver()
	observerErr, err := observer.Start(observerCtx)
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", cfg.ListenPort, err)
	}

	e.setStatus(func() { e.phase = PhaseRegistering })

	req := relay.RegisterRequest{
		PublicHost:       publicHost,
		PublicPort:       cfg.ListenPort,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         identity.ClientID,
		RealityPublicKey: identity.RealityPublicKey,
		ShortID:          identity.ShortID,
		ServerName:       cfg.ServerName,
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		MaxSessions:      cfg.MaxSessions,
		MaxMbps:          cfg.MaxMbps,
		RelayVersion:     cfg.Version,
		Label:            label,
	}
	desc, err := registerWithRetry(ctx, broker, req)
	if err != nil {
		return err
	}
	e.logf("registered with the broker as %q (%s)", desc.Label, desc.ID)
	e.setStatus(func() {
		e.phase = PhaseOnline
		e.transport = relay.TransportDirect
		e.relayID = desc.ID
		e.publicHost = desc.PublicHost
		e.publicPort = desc.PublicPort
		e.lastErr = ""
		e.onlineAt = time.Now()
	})

	heartbeat := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeat.Stop()
	// Home IPv6 prefixes rotate; re-detect periodically and restart the session
	// (fresh registration) when the address moves, because heartbeats never
	// update public_host broker-side. The comparison must be like-against-like:
	// publicHost may be a hub-observed source address (in auto mode it can even
	// be IPv4, or an OS temporary/privacy IPv6), which would never equal the
	// locally-enumerated address and would restart the session every tick. So
	// we baseline against the same source the recheck uses, and only when it
	// yields an address at all (no global IPv6 ⇒ nothing to watch here).
	const ipRecheckInterval = 5 * time.Minute
	ipBaseline, _ := detectPublicIPv6()
	ipRecheck := time.NewTicker(ipRecheckInterval)
	defer ipRecheck.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-xrayErr:
			if err == nil {
				return errors.New("xray exited")
			}
			return fmt.Errorf("xray exited: %w", err)
		case err, ok := <-observerErr:
			if !ok {
				observerErr = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("connection listener stopped: %w", err)
			}
		case <-ipRecheck.C:
			detected, err := detectPublicIPv6()
			if err == nil && ipBaseline != "" && detected != ipBaseline {
				e.logf("public IPv6 changed %s → %s; re-registering", ipBaseline, detected)
				return errPublicIPChanged
			}
		case <-heartbeat.C:
			if err := broker.Heartbeat(ctx, desc.ID); err != nil {
				if !volunteer.IsRelayNotFound(err) {
					e.logf("heartbeat failed: %v", err)
					continue
				}
				updated, regErr := broker.Register(ctx, req)
				if regErr != nil {
					e.logf("re-register after expired lease failed: %v", regErr)
					continue
				}
				desc = updated
				e.logf("re-registered with the broker as %q (%s)", desc.Label, desc.ID)
				e.setStatus(func() { e.relayID = desc.ID })
			}
		}
	}
}

// registerWithRetry gives startup registration a few tries with short backoff
// (honouring 429 Retry-After) before failing the session to the supervisor.
func registerWithRetry(ctx context.Context, broker *volunteer.BrokerClient, req relay.RegisterRequest) (relay.Descriptor, error) {
	const attempts = 3
	backoff := 2 * time.Second
	var lastErr error
	for i := 0; i < attempts; i++ {
		desc, err := broker.Register(ctx, req)
		if err == nil {
			return desc, nil
		}
		if ctx.Err() != nil {
			return relay.Descriptor{}, ctx.Err()
		}
		lastErr = err
		wait := backoff
		var apiErr *volunteer.APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter != "" {
			if secs, parseErr := strconv.Atoi(apiErr.RetryAfter); parseErr == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return relay.Descriptor{}, ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
	return relay.Descriptor{}, fmt.Errorf("register with broker: %w", lastErr)
}

func (e *Engine) runTunnelSession(ctx context.Context, cfg Config, label string, identity Identity) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	loopHost, loopPort, err := volunteer.ReserveLoopbackTCPPort()
	if err != nil {
		return err
	}

	xrayCmd, xrayErr, err := e.startXray(sessionCtx, cfg, identity, loopHost, loopPort)
	if err != nil {
		return err
	}
	defer stopProcess(xrayCmd, xrayErr)

	e.setStatus(func() { e.phase = PhaseRegistering })

	client := &tunnel.Client{
		HubAddr:   cfg.HubAddr,
		TLSConfig: hubTLSConfig(cfg),
		Hello: tunnel.HelloFrame{
			Token:            cfg.Token,
			RealityPublicKey: identity.RealityPublicKey,
			ShortID:          identity.ShortID,
			ServerName:       cfg.ServerName,
			ClientID:         identity.ClientID,
			Flow:             relay.FlowVision,
			ExitMode:         relay.ExitModeDirect,
			MaxSessions:      cfg.MaxSessions,
			MaxMbps:          cfg.MaxMbps,
			Label:            label,
			RelayVersion:     cfg.Version,
			StreamTyping:     true,
			PunchCapable:     cfg.PunchCapable,
		},
		TargetHost: loopHost,
		TargetPort: loopPort,
		Stats:      &e.tunnelStats,
		OnRegistered: func(ack tunnel.HelloAckFrame) {
			e.logf("relay published via hub at %s", net.JoinHostPort(ack.PublicHost, strconv.Itoa(ack.PublicPort)))
			e.setStatus(func() {
				e.phase = PhaseOnline
				e.transport = relay.TransportTunnel
				e.relayID = ack.RelayID
				e.publicHost = ack.PublicHost
				e.publicPort = ack.PublicPort
				e.lastErr = ""
				if e.onlineAt.IsZero() {
					e.onlineAt = time.Now()
				}
			})
		},
		OnDisconnected: func(err error, retryIn time.Duration) {
			if err != nil {
				e.logf("hub connection lost (%v); reconnecting in %s", err, retryIn)
			}
			e.setStatus(func() {
				e.phase = PhaseRegistering
				if err != nil {
					e.lastErr = err.Error()
				}
			})
		},
	}

	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(sessionCtx) }()
	e.logf("connecting to relay hub %s", cfg.HubAddr)

	// In auto mode, keep re-checking whether this relay host could serve directly
	// and switch off the hub when it can — so a hub outage or a newly-opened
	// port doesn't leave a directly reachable relay tunnelling forever.
	becameDirect := make(chan struct{}, 1)
	if cfg.Mode == ModeAuto {
		go e.watchForModeChange(sessionCtx, cfg, ModeTunnel, becameDirect)
	}

	select {
	case <-ctx.Done():
		cancel()
		<-clientDone
		return nil
	case <-becameDirect:
		e.logf("switching from tunnel to direct mode")
		cancel()
		<-clientDone
		return errReresolve
	case err := <-xrayErr:
		cancel()
		<-clientDone
		if err != nil {
			return fmt.Errorf("xray exited: %w", err)
		}
		return errors.New("xray exited")
	case err := <-clientDone:
		if err != nil {
			return fmt.Errorf("hub connection stopped: %w", err)
		}
		return errors.New("hub connection stopped")
	}
}

// hubTLSConfig is the TLS config for the tunnel control dial: the shared hub
// trust policy plus the ServerName the raw TLS dialer needs. nil means plaintext
// (test/dev only).
func hubTLSConfig(cfg Config) *tls.Config {
	if cfg.HubPlaintext {
		return nil
	}
	host, _, err := net.SplitHostPort(cfg.HubAddr)
	if err != nil {
		host = cfg.HubAddr
	}
	tc := hubTLSClientConfig(cfg)
	// Explicit for the raw tls.Dialer (the probe's HTTP transport sets this
	// per-request from the URL instead). Ignored on the pinned path, which
	// verifies the leaf by fingerprint rather than hostname.
	tc.ServerName = host
	return tc
}

// hubTLSClientConfig is the single source of truth for how the relay trusts
// the hub's TLS certificate, shared by the tunnel control dial (hubTLSConfig)
// and the reachability probe's HTTP client (probeClient). When a fingerprint is
// pinned it verifies the exact leaf (the hub self-signs on a bare IP, so
// CA/hostname verification cannot succeed); otherwise it honours HubInsecure
// (test-only), else standard verification. It sets no ServerName, so each caller
// can supply its own. Returns a fresh config per call — callers may mutate it.
func hubTLSClientConfig(cfg Config) *tls.Config {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin := normalizeFingerprint(cfg.HubCertFingerprint); pin != "" {
		// Skip the chain/hostname check and require the presented leaf to match
		// the expected SHA-256. MITM-proof without a CA — a forged cert has a
		// different key and therefore a different fingerprint.
		tc.InsecureSkipVerify = true //nolint:gosec // not insecure: VerifyPeerCertificate pins the exact leaf below
		tc.VerifyPeerCertificate = pinnedLeafVerifier(pin)
		return tc
	}
	tc.InsecureSkipVerify = cfg.HubInsecure //nolint:gosec // test-only, mirrors cmd/volunteer -hub-insecure
	return tc
}

// normalizeFingerprint lowercases a SHA-256 hex fingerprint and strips the
// colon/space separators that openssl-style output uses, so "AB:CD:.." and
// "abcd.." compare equal.
func normalizeFingerprint(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	fp = strings.ReplaceAll(fp, ":", "")
	fp = strings.ReplaceAll(fp, " ", "")
	return fp
}

// pinnedLeafVerifier returns a tls VerifyPeerCertificate that accepts the
// connection only when the leaf certificate's SHA-256 equals want.
func pinnedLeafVerifier(want string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("hub presented no TLS certificate")
		}
		sum := sha256.Sum256(rawCerts[0])
		got := hex.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return fmt.Errorf("hub certificate fingerprint mismatch (expected %s, got %s)", want, got)
		}
		return nil
	}
}

// stopProcess interrupts the xray child and escalates to Kill; nil cmd (xray
// disabled) is a no-op.
func stopProcess(cmd *exec.Cmd, errCh <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		// Windows has no Interrupt for child processes; go straight to Kill.
		_ = cmd.Process.Kill()
	}
	select {
	case <-errCh:
		return
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
	}
	select {
	case <-errCh:
	case <-time.After(time.Second):
	}
}
