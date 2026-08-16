// Command relay runs an OpenRung relay from the command line. It is a
// flag-parsing frontend: every flag and OPENRUNG_* variable maps onto an
// internal/relayruntime/engine Config, the engine owns all orchestration
// (probing, xray supervision, registration, heartbeat, tunnelling), and this
// package only renders its status back to the console.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"openrung/internal/buildinfo"
	"openrung/internal/relay"
	"openrung/internal/relayruntime"
	"openrung/internal/relayruntime/engine"
)

//go:embed VERSION
var baseVersion string

func main() {
	identitySeed := os.Getenv(relayruntime.IdentitySeedEnvironmentVariable)
	// Retain environment-based configuration without leaving the long-lived
	// identity seed available to Xray or any other child process. Explicit
	// -identity-seed still takes precedence when flags are parsed below.
	if err := os.Unsetenv(relayruntime.IdentitySeedEnvironmentVariable); err != nil {
		slog.Error("clear relay identity seed from environment", "error", err)
		os.Exit(1)
	}

	flags := &cliFlags{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flags.register(fs, identitySeed)
	_ = fs.Parse(os.Args[1:])

	if flags.showVersion {
		fmt.Println(versionInfo())
		return
	}
	if flags.printLabel {
		fmt.Println(relayruntime.GenerateLabel())
		return
	}

	cfg, err := flags.engineConfig()
	if err != nil {
		slog.Error("invalid relay config", "error", err)
		os.Exit(2)
	}
	eng := engine.New(cfg, engine.Events{
		// Engine progress and xray's own output go to stderr alongside the
		// structured log; the observer's per-connection lines stay on stdout,
		// where cmd/relay has always printed them.
		Log:      os.Stderr,
		OnStatus: (&consoleReporter{}).observe,
	})

	if flags.printConfigOnly {
		rendered, err := eng.RenderXrayConfig()
		if err != nil {
			slog.Error("invalid relay config", "error", err)
			os.Exit(2)
		}
		fmt.Println(string(rendered))
		return
	}

	// Arm the signal handler before anything starts, so a SIGTERM that lands
	// during startup still reaches Stop and reaps xray instead of killing the
	// relay and orphaning its child.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting relay", "version", buildinfo.Version(baseVersion), "revision", buildinfo.Revision())
	if err := eng.Start(); err != nil {
		slog.Error("invalid relay config", "error", err)
		os.Exit(2)
	}

	<-ctx.Done()
	stop()
	eng.Stop()
}

// cliFlags is the relay's command-line surface. Deployment scripts under
// deploy/ and the volunteer one-liner consume these flags and the OPENRUNG_*
// variables behind them, so the set is a compatibility contract: names,
// defaults, and meanings only ever gain members.
type cliFlags struct {
	showVersion     bool
	printLabel      bool
	printConfigOnly bool

	broker             string
	registrationToken  string
	foundationToken    string
	label              string
	nodeClass          string
	xrayPath           string
	listenHost         string
	listenPort         int
	publicHost         string
	publicPort         int
	serverName         string
	realityDest        string
	clientID           string
	realityPrivateKey  string
	realityPublicKey   string
	shortID            string
	identitySeed       string
	wssFronts          string
	maxSessions        int
	maxMbps            int
	heartbeatInterval  time.Duration
	configOut          string
	connectionLog      bool
	skipXrayRun        bool
	mode               string
	tunnel             bool
	hubAddr            string
	hubHTTP            string
	hubCertFingerprint string
	hubTLS             bool
	hubInsecure        bool
	punch              bool
}

func (f *cliFlags) register(fs *flag.FlagSet, identitySeed string) {
	fs.BoolVar(&f.showVersion, "version", false, "print relay version and exit")
	fs.BoolVar(&f.printLabel, "print-label", false, "print one random adjective-noun label and exit; provisioning scripts use this to name a relay from the binary's own vocabulary instead of keeping a copy of the word lists")
	fs.StringVar(&f.broker, "broker", "http://localhost:8080", "broker base URL")
	fs.StringVar(&f.registrationToken, "registration-token", os.Getenv("OPENRUNG_VOLUNTEER_TOKEN"), "volunteer-class relay registration token")
	fs.StringVar(&f.label, "label", os.Getenv("OPENRUNG_LABEL"), "human-readable relay label shown in the broker; a random adjective-noun is generated when empty")
	fs.StringVar(&f.nodeClass, "node-class", os.Getenv("OPENRUNG_NODE_CLASS"), "relay operator class: volunteer (default) or foundation. For a foundation relay prefer -foundation-token, which sets this and forces direct mode / https automatically; a bare -node-class=foundation still needs direct mode, the foundation token as the bearer, and an https broker")
	fs.StringVar(&f.foundationToken, "foundation-token", os.Getenv("OPENRUNG_FOUNDATION_TOKEN"), "foundation registration token; presenting it runs this as a foundation relay — it forces foundation class, direct mode, an https broker, and redirect refusal, so no separate -node-class is needed")
	fs.StringVar(&f.xrayPath, "xray", "xray", "path to xray binary")
	fs.StringVar(&f.listenHost, "listen-host", "::", "local listen host; with connection logging, :: listens on both IPv6 and IPv4 through the observer")
	fs.IntVar(&f.listenPort, "listen-port", 443, "local listen port")
	fs.StringVar(&f.publicHost, "public-host", "", "public hostname or IP clients can reach; defaults to the relay host's first global IPv6 address")
	fs.IntVar(&f.publicPort, "public-port", 443, "public port clients can reach")
	fs.StringVar(&f.serverName, "server-name", "www.cloudflare.com", "Reality server name")
	fs.StringVar(&f.realityDest, "reality-dest", "www.cloudflare.com:443", "Reality dest")
	fs.StringVar(&f.clientID, "client-id", "", "VLESS client UUID; generated when empty")
	fs.StringVar(&f.realityPrivateKey, "reality-private-key", "", "Reality private key; generated with xray x25519 when empty")
	fs.StringVar(&f.realityPublicKey, "reality-public-key", "", "Reality public key; generated with xray x25519 when empty")
	fs.StringVar(&f.shortID, "short-id", "", "Reality short ID; generated when empty")
	fs.StringVar(&f.identitySeed, "identity-seed", identitySeed, "base64 32-byte Ed25519 seed for the relay's stable identity (spec openrung-relay-identity-v1); the broker derives the relay ID from it, so a pinned seed keeps the same ID across restarts. Generated per process when empty")
	fs.StringVar(&f.wssFronts, "wss-fronts", os.Getenv("OPENRUNG_WSS_FRONTS"), "comma-separated per-relay CDN fronts as front-id=wss://cdn.example/api/v1/wss-bridge (Foundation direct mode on port 443 with an explicit identity seed only)")
	fs.IntVar(&f.maxSessions, "max-sessions", relayruntime.DefaultMaxSessions, "advertised max client sessions")
	fs.IntVar(&f.maxMbps, "max-mbps", relayruntime.DefaultMaxMbps, "advertised max Mbps")
	fs.DurationVar(&f.heartbeatInterval, "heartbeat-interval", 30*time.Second, "broker heartbeat interval")
	fs.StringVar(&f.configOut, "config-out", "", "write generated Xray config to this path")
	fs.BoolVar(&f.connectionLog, "connection-log", true, "print colored client connect and disconnect events")
	fs.BoolVar(&f.printConfigOnly, "print-config-only", false, "print generated Xray config and exit")
	fs.BoolVar(&f.skipXrayRun, "skip-xray-run", false, "register and heartbeat without launching xray")
	fs.StringVar(&f.mode, "mode", os.Getenv("OPENRUNG_MODE"), "connection mode: auto (probe reachability and pick direct/tunnel), direct, or tunnel; defaults to auto when -hub is set, else direct")
	fs.BoolVar(&f.tunnel, "tunnel", boolEnv("OPENRUNG_TUNNEL"), "force CGNAT reverse-tunnel mode (alias for -mode tunnel)")
	fs.StringVar(&f.hubAddr, "hub", os.Getenv("OPENRUNG_HUB_ADDR"), "relay hub control address (host:port) for tunnel/auto mode")
	fs.StringVar(&f.hubHTTP, "hub-http", os.Getenv("OPENRUNG_HUB_HTTP_URL"), "relay hub HTTP API base URL for reachability probing; defaults to http://<hub-host>:9444")
	fs.StringVar(&f.hubCertFingerprint, "hub-cert-fingerprint", os.Getenv("OPENRUNG_HUB_CERT_FINGERPRINT"), "pin the relay hub's TLS leaf certificate to this SHA-256 fingerprint (hex; colons and case ignored). A hub on a bare IP self-signs, so CA verification cannot succeed; pinning the exact leaf is MITM-proof without a CA and is the production-safe alternative to -hub-insecure. Empty keeps standard verification")
	fs.BoolVar(&f.hubTLS, "hub-tls", true, "dial the relay hub over TLS in tunnel mode")
	fs.BoolVar(&f.hubInsecure, "hub-insecure", false, "skip TLS certificate verification when dialing the relay hub (testing only)")
	fs.BoolVar(&f.punch, "punch", !boolEnv("OPENRUNG_PUNCH_DISABLE"), "offer NAT hole punching so clients can connect directly (tunnel mode; requires a punch-capable hub)")
}

// engineConfig maps the parsed flags onto the engine. Everything it rejects is
// a malformed flag value; every posture rule (foundation, WSS, mode) belongs to
// the engine and is enforced by Start.
func (f *cliFlags) engineConfig() (engine.Config, error) {
	mode := normalizeMode(f.mode, f.tunnel, f.hubAddr)
	// The engine lets a hubless auto config degrade to direct, which suits a
	// GUI whose user has not configured a hub yet. An operator who typed
	// -mode auto asked for reachability probing, so say it cannot run rather
	// than silently serving direct. (normalizeMode only returns auto for an
	// explicit request or a configured hub, so this catches only the former.)
	if mode == engine.ModeAuto && f.hubAddr == "" {
		return engine.Config{}, fmt.Errorf("hub is required in auto mode for reachability probing (set -hub or use -mode direct)")
	}
	// Zero reads to the engine as "advertise the listen port", which is a
	// sensible default for a programmatic caller but never what a flag that
	// defaults to 443 meant.
	if f.publicPort < 1 || f.publicPort > 65535 {
		return engine.Config{}, fmt.Errorf("public-port must be between 1 and 65535")
	}
	fronts, err := parseWSSFrontsFlag(f.wssFronts)
	if err != nil {
		return engine.Config{}, fmt.Errorf("invalid wss-fronts: %w", err)
	}
	// The engine regenerates an unreadable identity seed rather than failing,
	// which is right for a GUI volunteer who cannot hand-repair identity.json
	// but wrong for a server: a mistyped -identity-seed would silently churn
	// the relay ID. Reject it here, while it is still a bad flag value.
	if strings.TrimSpace(f.identitySeed) != "" {
		if _, err := relay.ParseIdentitySeed(f.identitySeed); err != nil {
			return engine.Config{}, fmt.Errorf("parse identity seed: %w", err)
		}
	}
	// Per-connection lines carry client addresses, so -connection-log=false
	// must leave the engine with nowhere to write them at all.
	var connectionLog io.Writer
	if f.connectionLog {
		connectionLog = os.Stdout
	}
	configPath := f.configOut
	if configPath == "" {
		configPath = filepath.Join(os.TempDir(), "openrung-xray-config.json")
	}
	return engine.Config{
		BrokerURL:           f.broker,
		Token:               f.registrationToken,
		FoundationToken:     f.foundationToken,
		NodeClass:           f.nodeClass,
		Label:               f.label,
		PublicHost:          f.publicHost,
		PublicPort:          f.publicPort,
		XrayPath:            f.xrayPath,
		ListenHost:          f.listenHost,
		ListenPort:          f.listenPort,
		Mode:                mode,
		HubAddr:             f.hubAddr,
		HubHTTPURL:          f.hubHTTP,
		HubCertFingerprint:  f.hubCertFingerprint,
		HubInsecure:         f.hubInsecure,
		HubPlaintext:        !f.hubTLS,
		ServerName:          f.serverName,
		RealityDest:         f.realityDest,
		MaxSessions:         f.maxSessions,
		MaxMbps:             f.maxMbps,
		HeartbeatInterval:   f.heartbeatInterval,
		WSSFronts:           fronts,
		ConnectionLogOutput: connectionLog,
		Identity: engine.Identity{
			ClientID:          f.clientID,
			RealityPrivateKey: f.realityPrivateKey,
			RealityPublicKey:  f.realityPublicKey,
			ShortID:           f.shortID,
			IdentitySeed:      f.identitySeed,
		},
		ConfigPath:   configPath,
		Version:      reportedRelayVersion(),
		PunchCapable: f.punch,
		DisableXray:  f.skipXrayRun,
	}, nil
}

// normalizeMode resolves the requested mode. An explicit -mode wins; otherwise
// -tunnel forces tunnel, a configured hub enables auto-detection, and the final
// fallback is direct (preserving the historical default for hubless setups).
func normalizeMode(mode string, tunnelFlag bool, hubAddr string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case engine.ModeDirect:
		return engine.ModeDirect
	case engine.ModeTunnel:
		return engine.ModeTunnel
	case engine.ModeAuto:
		return engine.ModeAuto
	case "":
		switch {
		case tunnelFlag:
			return engine.ModeTunnel
		case hubAddr != "":
			return engine.ModeAuto
		default:
			return engine.ModeDirect
		}
	default:
		return mode // invalid; rejected by the engine
	}
}

func parseWSSFrontsFlag(raw string) ([]relay.WSSFrontDescriptor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ",")
	fronts := make([]relay.WSSFrontDescriptor, 0, len(entries))
	for index, rawEntry := range entries {
		entry := strings.TrimSpace(rawEntry)
		id, frontURL, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(frontURL) == "" {
			return nil, fmt.Errorf("wss-fronts entry %d must use front-id=wss://cdn.example%s", index, relay.WSSBridgePath)
		}
		fronts = append(fronts, relay.WSSFrontDescriptor{
			ID:              id,
			URL:             frontURL,
			ProtocolVersion: relay.WSSProtocolVersion,
		})
	}
	return relay.NormalizeWSSFronts(fronts)
}

func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// consoleReporter renders engine status transitions as the operator-facing log
// lines cmd/relay has always printed. deploy/relay/foundation-up.sh and
// foundation-wss-host.sh poll container logs for "registered relay", so that
// message is a deployment contract rather than decoration.
type consoleReporter struct {
	mu sync.Mutex
	// registrations is the engine's count as last seen while online. The relay
	// ID cannot stand in for it: the broker derives the ID from the identity
	// key, so re-registering after an expired lease returns the same one and a
	// diff of RelayID would report nothing.
	registrations uint64
	lastPhase     engine.Phase
}

func (c *consoleReporter) observe(status engine.Status) {
	c.mu.Lock()
	previous, previousPhase := c.registrations, c.lastPhase
	// Leaving online ends the registration: whatever comes back is a fresh one,
	// not a renewal of the lease that just lapsed.
	c.registrations = 0
	if status.Phase == engine.PhaseOnline {
		c.registrations = status.Registrations
	}
	c.lastPhase = status.Phase
	c.mu.Unlock()

	switch {
	case status.Phase == engine.PhaseOnline && status.Registrations > previous:
		public := net.JoinHostPort(status.PublicHost, strconv.Itoa(status.PublicPort))
		switch {
		case status.Transport == relay.TransportTunnel:
			slog.Info("relay published via hub", "relay_id", status.RelayID, "label", status.Label, "public", public)
		case previous == 0:
			slog.Info("registered relay", "id", status.RelayID, "label", status.Label, "public", public)
		default:
			slog.Info("re-registered relay", "id", status.RelayID, "label", status.Label, "public", public)
		}
	case status.Phase == engine.PhaseRetrying && previousPhase != engine.PhaseRetrying:
		slog.Warn("relay stopped, retrying", "error", status.LastError)
	}
}

// reportedRelayVersion is the relay_version identity sent to the broker and
// hub, not just display output.
func reportedRelayVersion() string {
	return "relay/" + buildinfo.Version(baseVersion)
}

func versionInfo() string {
	return fmt.Sprintf("%s revision=%s", reportedRelayVersion(), buildinfo.Revision())
}
