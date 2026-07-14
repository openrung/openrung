package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openrung/internal/relay"
	"openrung/internal/tunnel"
	"openrung/internal/volunteer"
)

const version = "dev"

func main() {
	var cfg cliConfig
	flag.StringVar(&cfg.BrokerURL, "broker", "http://localhost:8080", "broker base URL")
	flag.StringVar(&cfg.RegistrationToken, "registration-token", os.Getenv("OPENRUNG_VOLUNTEER_TOKEN"), "volunteer-class relay registration token")
	flag.StringVar(&cfg.Label, "label", os.Getenv("OPENRUNG_LABEL"), "human-readable relay label shown in the broker; a random adjective-noun is generated when empty")
	flag.StringVar(&cfg.NodeClass, "node-class", os.Getenv("OPENRUNG_NODE_CLASS"), "relay operator class: volunteer (default) or foundation. For a foundation relay prefer -foundation-token, which sets this and forces direct mode / https automatically; a bare -node-class=foundation still needs direct mode, the foundation token as the bearer, and an https broker")
	flag.StringVar(&cfg.FoundationToken, "foundation-token", os.Getenv("OPENRUNG_FOUNDATION_TOKEN"), "foundation registration token; presenting it runs this as a foundation relay — it forces foundation class, direct mode, an https broker, and redirect refusal, so no separate -node-class is needed")
	flag.StringVar(&cfg.XrayPath, "xray", "xray", "path to xray binary")
	flag.StringVar(&cfg.ListenHost, "listen-host", "::", "local listen host; with connection logging, :: listens on both IPv6 and IPv4 through the observer")
	flag.IntVar(&cfg.ListenPort, "listen-port", 443, "local listen port")
	flag.StringVar(&cfg.PublicHost, "public-host", "", "public hostname or IP clients can reach; defaults to the relay host's first global IPv6 address")
	flag.IntVar(&cfg.PublicPort, "public-port", 443, "public port clients can reach")
	flag.StringVar(&cfg.ServerName, "server-name", "www.cloudflare.com", "Reality server name")
	flag.StringVar(&cfg.RealityDest, "reality-dest", "www.cloudflare.com:443", "Reality dest")
	flag.StringVar(&cfg.ClientID, "client-id", "", "VLESS client UUID; generated when empty")
	flag.StringVar(&cfg.RealityPrivateKey, "reality-private-key", "", "Reality private key; generated with xray x25519 when empty")
	flag.StringVar(&cfg.RealityPublicKey, "reality-public-key", "", "Reality public key; generated with xray x25519 when empty")
	flag.StringVar(&cfg.ShortID, "short-id", "", "Reality short ID; generated when empty")
	flag.IntVar(&cfg.MaxSessions, "max-sessions", 8, "advertised max client sessions")
	flag.IntVar(&cfg.MaxMbps, "max-mbps", 20, "advertised max Mbps")
	flag.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", 30*time.Second, "broker heartbeat interval")
	flag.StringVar(&cfg.ConfigOut, "config-out", "", "write generated Xray config to this path")
	flag.BoolVar(&cfg.ConnectionLog, "connection-log", true, "print colored client connect and disconnect events")
	flag.BoolVar(&cfg.PrintConfigOnly, "print-config-only", false, "print generated Xray config and exit")
	flag.BoolVar(&cfg.SkipXrayRun, "skip-xray-run", false, "register and heartbeat without launching xray")
	flag.StringVar(&cfg.Mode, "mode", os.Getenv("OPENRUNG_MODE"), "connection mode: auto (probe reachability and pick direct/tunnel), direct, or tunnel; defaults to auto when -hub is set, else direct")
	flag.BoolVar(&cfg.TunnelMode, "tunnel", boolEnv("OPENRUNG_TUNNEL"), "force CGNAT reverse-tunnel mode (alias for -mode tunnel)")
	flag.StringVar(&cfg.HubAddr, "hub", os.Getenv("OPENRUNG_HUB_ADDR"), "relay hub control address (host:port) for tunnel/auto mode")
	flag.StringVar(&cfg.HubHTTP, "hub-http", os.Getenv("OPENRUNG_HUB_HTTP_URL"), "relay hub HTTP API base URL for reachability probing; defaults to http://<hub-host>:9444")
	flag.BoolVar(&cfg.HubTLS, "hub-tls", true, "dial the relay hub over TLS in tunnel mode")
	flag.BoolVar(&cfg.HubInsecure, "hub-insecure", false, "skip TLS certificate verification when dialing the relay hub (testing only)")
	flag.BoolVar(&cfg.Punch, "punch", !boolEnv("OPENRUNG_PUNCH_DISABLE"), "offer NAT hole punching so clients can connect directly (tunnel mode; requires a punch-capable hub)")
	flag.Parse()

	cfg.Mode = normalizeMode(cfg.Mode, cfg.TunnelMode, cfg.HubAddr)

	if err := cfg.ApplyDefaults(); err != nil {
		slog.Error("invalid relay config", "error", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid relay config", "error", err)
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		slog.Error("relay stopped", "error", err)
		os.Exit(1)
	}
}

type cliConfig struct {
	BrokerURL         string
	RegistrationToken string
	FoundationToken   string
	Label             string
	NodeClass         string
	XrayPath          string
	ListenHost        string
	ListenPort        int
	PublicHost        string
	PublicPort        int
	ServerName        string
	RealityDest       string
	ClientID          string
	RealityPrivateKey string
	RealityPublicKey  string
	ShortID           string
	MaxSessions       int
	MaxMbps           int
	HeartbeatInterval time.Duration
	HTTPClient        *http.Client
	ConfigOut         string
	ConnectionLog     bool
	PrintConfigOnly   bool
	SkipXrayRun       bool
	Mode              string
	TunnelMode        bool
	HubAddr           string
	HubHTTP           string
	HubTLS            bool
	HubInsecure       bool
	Punch             bool
}

// normalizeMode resolves the requested mode. An explicit -mode wins; otherwise
// -tunnel forces tunnel, a configured hub enables auto-detection, and the final
// fallback is direct (preserving the historical default for hubless setups).
func normalizeMode(mode string, tunnelFlag bool, hubAddr string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "direct":
		return "direct"
	case "tunnel":
		return "tunnel"
	case "auto":
		return "auto"
	case "":
		switch {
		case tunnelFlag:
			return "tunnel"
		case hubAddr != "":
			return "auto"
		default:
			return "direct"
		}
	default:
		return mode // invalid; rejected by Validate
	}
}

// requireDirectModeForFoundation prevents a foundation credential from ever
// entering the hub-based auto/tunnel paths. Auto probing sends the registration
// token to the hub before it chooses a mode, so even an eventually-direct result
// is not safe for the foundation token.
func requireDirectModeForFoundation(nodeClass, mode string) error {
	if nodeClass == relay.NodeClassFoundation && mode != "direct" {
		return fmt.Errorf("node-class foundation requires direct mode: auto and tunnel send the registration token to the relay hub. Use -mode direct against a TLS broker, or set -foundation-token, which forces direct mode")
	}
	return nil
}

// applyFoundationTokenPosture makes a foundation token a self-contained relay
// posture: presenting it forces foundation class and direct mode (HTTPS and
// redirect refusal follow from the foundation bearer in brokerClient). Setting
// OPENRUNG_FOUNDATION_TOKEN is therefore sufficient — no separate
// OPENRUNG_NODE_CLASS. An explicit non-foundation node-class is a contradiction
// and rejected; a non-direct mode is overridden to direct (direct is the only
// mode that never routes the token through a hub). Called from both
// ApplyDefaults and run so the posture holds even for a caller that skips
// ApplyDefaults.
func applyFoundationTokenPosture(c *cliConfig) error {
	if c.FoundationToken == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.NodeClass)) {
	case "", relay.NodeClassFoundation:
	default:
		return fmt.Errorf("node-class %q conflicts with a foundation token, which already forces foundation class; unset -node-class/OPENRUNG_NODE_CLASS", c.NodeClass)
	}
	c.NodeClass = relay.NodeClassFoundation
	c.Mode = "direct"
	return nil
}

func (c *cliConfig) ApplyDefaults() error {
	if err := applyFoundationTokenPosture(c); err != nil {
		return err
	}
	if c.Mode == "" {
		c.Mode = normalizeMode("", c.TunnelMode, c.HubAddr)
	}
	if c.Label == "" {
		c.Label = volunteer.GenerateLabel()
	} else {
		normalized, err := relay.NormalizeLabel(c.Label)
		if err != nil {
			return fmt.Errorf("invalid label: %w", err)
		}
		c.Label = normalized
	}
	nodeClass, err := relay.NormalizeNodeClass(c.NodeClass)
	if err != nil {
		return fmt.Errorf("invalid node-class: %w", err)
	}
	c.NodeClass = nodeClass
	if c.Mode == "tunnel" || c.Mode == "auto" {
		// Tunnel mode gets its public endpoint from the hub; auto mode resolves it
		// at runtime from the reachability probe. Neither needs a public host now.
		return nil
	}
	if c.PublicHost != "" || c.PrintConfigOnly {
		return nil
	}
	publicIPv6, err := volunteer.DefaultPublicIPv6Address()
	if err != nil {
		return fmt.Errorf("public-host is required when no global IPv6 address can be auto-detected: %w", err)
	}
	c.PublicHost = publicIPv6
	return nil
}

func (c cliConfig) Validate() error {
	mode := c.Mode
	if mode == "" {
		mode = normalizeMode("", c.TunnelMode, c.HubAddr)
	}
	if err := requireDirectModeForFoundation(c.NodeClass, mode); err != nil {
		return err
	}
	switch mode {
	case "tunnel":
		if c.HubAddr == "" {
			return fmt.Errorf("hub is required in tunnel mode (set -hub or OPENRUNG_HUB_ADDR)")
		}
		if c.MaxSessions < 1 {
			return fmt.Errorf("max-sessions must be at least 1")
		}
		if c.MaxMbps < 1 {
			return fmt.Errorf("max-mbps must be at least 1")
		}
		return nil
	case "auto":
		if c.HubAddr == "" {
			return fmt.Errorf("hub is required in auto mode for reachability probing (set -hub or use -mode direct)")
		}
		if c.BrokerURL == "" {
			return fmt.Errorf("broker is required")
		}
		if c.ListenPort < 1 || c.ListenPort > 65535 {
			return fmt.Errorf("listen-port must be between 1 and 65535")
		}
		if c.MaxSessions < 1 {
			return fmt.Errorf("max-sessions must be at least 1")
		}
		if c.MaxMbps < 1 {
			return fmt.Errorf("max-mbps must be at least 1")
		}
		if c.HeartbeatInterval < 5*time.Second {
			return fmt.Errorf("heartbeat-interval must be at least 5s")
		}
		// Auto can resolve to direct, which reuses the observer's listen host.
		if isDualListenHost(c.ListenHost) && (!c.ConnectionLog || c.PrintConfigOnly || c.SkipXrayRun) {
			return fmt.Errorf("listen-host=dual requires connection-log=true and a running xray process")
		}
		return nil
	case "direct":
		if c.BrokerURL == "" {
			return fmt.Errorf("broker is required")
		}
		if c.PublicHost == "" && !c.PrintConfigOnly {
			return fmt.Errorf("public-host is required")
		}
		if c.ListenPort < 1 || c.ListenPort > 65535 {
			return fmt.Errorf("listen-port must be between 1 and 65535")
		}
		if c.PublicPort < 1 || c.PublicPort > 65535 {
			return fmt.Errorf("public-port must be between 1 and 65535")
		}
		if c.MaxSessions < 1 {
			return fmt.Errorf("max-sessions must be at least 1")
		}
		if c.MaxMbps < 1 {
			return fmt.Errorf("max-mbps must be at least 1")
		}
		if c.HeartbeatInterval < 5*time.Second {
			return fmt.Errorf("heartbeat-interval must be at least 5s")
		}
		if isDualListenHost(c.ListenHost) && (!c.ConnectionLog || c.PrintConfigOnly || c.SkipXrayRun) {
			return fmt.Errorf("listen-host=dual requires connection-log=true and a running xray process")
		}
		return nil
	default:
		return fmt.Errorf("mode must be auto, direct, or tunnel")
	}
}

func run(cfg cliConfig) error {
	if err := applyFoundationTokenPosture(&cfg); err != nil {
		return err
	}
	nodeClass, err := relay.NormalizeNodeClass(cfg.NodeClass)
	if err != nil {
		return fmt.Errorf("invalid node-class: %w", err)
	}
	cfg.NodeClass = nodeClass

	mode := cfg.Mode
	if mode == "" {
		mode = normalizeMode("", cfg.TunnelMode, cfg.HubAddr)
	}
	if err := requireDirectModeForFoundation(cfg.NodeClass, mode); err != nil {
		// Keep this guard in the runtime path as well as Validate. Besides making
		// run safe for programmatic callers, it guarantees rejection before
		// resolveAutoMode can transmit the foundation token in a hub probe.
		return err
	}
	cfg.Mode = mode

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case cfg.Mode == "auto" && cfg.PrintConfigOnly:
		// Skip probing for a config dump; print the direct-form config. (Tunnel
		// mode would rebind Xray to a loopback port at runtime, but that is
		// resolved live, not knowable at print time.)
		cfg.TunnelMode = false
	case cfg.Mode == "auto":
		resolveAutoMode(ctx, &cfg)
	default:
		cfg.TunnelMode = cfg.Mode == "tunnel"
	}

	if cfg.TunnelMode {
		return runTunnelMode(ctx, cfg)
	}

	xrayCfg := cfg
	if cfg.ConnectionLog && !cfg.SkipXrayRun && !cfg.PrintConfigOnly {
		targetHost, targetPort, err := volunteer.ReserveLoopbackTCPPort()
		if err != nil {
			return err
		}
		xrayCfg.ListenHost = targetHost
		xrayCfg.ListenPort = targetPort
	}

	prepared, err := prepareRuntime(xrayCfg)
	if err != nil {
		return err
	}

	if cfg.PrintConfigOnly {
		fmt.Println(string(prepared.XrayConfig))
		return nil
	}

	configPath := cfg.ConfigOut
	if configPath == "" {
		configPath = filepath.Join(os.TempDir(), "openrung-xray-config.json")
	}
	if err := os.WriteFile(configPath, prepared.XrayConfig, 0o600); err != nil {
		return fmt.Errorf("write xray config: %w", err)
	}
	slog.Info("wrote xray config", "path", configPath)

	var xrayCmd *exec.Cmd
	var errCh <-chan error
	var observerErrCh <-chan error
	if !cfg.SkipXrayRun {
		xrayCmd = exec.CommandContext(ctx, cfg.XrayPath, "run", "-config", configPath)
		xrayCmd.Stdout = os.Stdout
		xrayCmd.Stderr = os.Stderr
		if err := xrayCmd.Start(); err != nil {
			return fmt.Errorf("start xray: %w", err)
		}
		waitCh := make(chan error, 1)
		go func() {
			waitCh <- xrayCmd.Wait()
		}()
		errCh = waitCh
		slog.Info("started xray", "pid", xrayCmd.Process.Pid)

		if cfg.ConnectionLog {
			observer := &volunteer.ConnectionObserver{
				ListenHost: cfg.ListenHost,
				ListenPort: cfg.ListenPort,
				TargetHost: xrayCfg.ListenHost,
				TargetPort: xrayCfg.ListenPort,
				Output:     os.Stdout,
			}
			observerErrCh, err = observer.Start(ctx)
			if err != nil {
				stopProcess(xrayCmd, errCh)
				return fmt.Errorf("start connection observer: %w", err)
			}
			slog.Info(
				"started connection observer",
				"listen",
				strings.Join(volunteer.ListenAddressesForHost(cfg.ListenHost, cfg.ListenPort), ","),
				"target",
				fmt.Sprintf("%s:%d", xrayCfg.ListenHost, xrayCfg.ListenPort),
				"note",
				"observer owns the public listen port and forwards to xray",
			)
		}
	}

	// Register only after xray and the public listener are up, so the broker
	// never advertises a relay that cannot serve: a port conflict or xray
	// failure aborts above, before any lease exists, which is what stops a
	// restart loop from perpetually refreshing a dead row. This applies to
	// foundation relays too — registration (with its node_class attestation)
	// stays here rather than gating the listener, because registering first
	// would publish a live foundation lease that a subsequent bind failure
	// could strand. Any registration failure, including a rejected foundation
	// attestation, tears the relay down below.
	broker := cfg.brokerClient()
	desc, err := register(ctx, broker, cfg, prepared)
	if err != nil {
		if xrayCmd != nil {
			stopProcess(xrayCmd, errCh)
		}
		return err
	}
	slog.Info("registered relay", "id", desc.ID, "label", desc.Label, "public", fmt.Sprintf("%s:%d", desc.PublicHost, desc.PublicPort))

	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if xrayCmd != nil {
				stopProcess(xrayCmd, errCh)
			}
			return nil
		case err := <-errCh:
			if err == nil {
				return fmt.Errorf("xray exited")
			}
			return fmt.Errorf("xray exited: %w", err)
		case err, ok := <-observerErrCh:
			if !ok {
				observerErrCh = nil
				continue
			}
			if err != nil {
				if xrayCmd != nil {
					stopProcess(xrayCmd, errCh)
				}
				return fmt.Errorf("connection observer stopped: %w", err)
			}
		case <-ticker.C:
			updatedDesc, reRegistered, err := heartbeatOrRegister(ctx, broker, cfg, prepared, desc)
			if err != nil {
				slog.Warn("heartbeat failed", "error", err)
				continue
			}
			desc = updatedDesc
			if reRegistered {
				slog.Info("re-registered relay", "id", desc.ID, "label", desc.Label, "public", fmt.Sprintf("%s:%d", desc.PublicHost, desc.PublicPort))
				continue
			}
			slog.Info("heartbeat ok", "id", desc.ID)
		}
	}
}

// runTunnelMode binds Xray to a loopback port and serves client traffic through
// a reverse tunnel to the relay hub. The hub registers the relay with the broker
// on the relay's behalf, so the relay never exposes a public port and
// never calls the broker directly.
func runTunnelMode(parent context.Context, cfg cliConfig) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Foundation never reaches tunnel mode: requireDirectModeForFoundation
	// (Validate and run) rejects any non-direct mode before dispatch.

	loopHost, loopPort, err := volunteer.ReserveLoopbackTCPPort()
	if err != nil {
		return err
	}
	xrayCfg := cfg
	xrayCfg.ListenHost = loopHost
	xrayCfg.ListenPort = loopPort

	prepared, err := prepareRuntime(xrayCfg)
	if err != nil {
		return err
	}

	if cfg.PrintConfigOnly {
		fmt.Println(string(prepared.XrayConfig))
		return nil
	}

	configPath := cfg.ConfigOut
	if configPath == "" {
		configPath = filepath.Join(os.TempDir(), "openrung-xray-config.json")
	}
	if err := os.WriteFile(configPath, prepared.XrayConfig, 0o600); err != nil {
		return fmt.Errorf("write xray config: %w", err)
	}
	slog.Info("wrote xray config", "path", configPath)

	var xrayCmd *exec.Cmd
	xrayErr := make(chan error, 1)
	if !cfg.SkipXrayRun {
		xrayCmd = exec.CommandContext(ctx, cfg.XrayPath, "run", "-config", configPath)
		xrayCmd.Stdout = os.Stdout
		xrayCmd.Stderr = os.Stderr
		if err := xrayCmd.Start(); err != nil {
			return fmt.Errorf("start xray: %w", err)
		}
		go func() { xrayErr <- xrayCmd.Wait() }()
		slog.Info("started xray", "pid", xrayCmd.Process.Pid, "listen", net.JoinHostPort(loopHost, strconv.Itoa(loopPort)))
	}

	client := &tunnel.Client{
		HubAddr:   cfg.HubAddr,
		TLSConfig: hubTLSConfig(cfg),
		Hello: tunnel.HelloFrame{
			Token:            cfg.RegistrationToken,
			RealityPublicKey: prepared.RealityPublicKey,
			ShortID:          prepared.ShortID,
			ServerName:       cfg.ServerName,
			ClientID:         prepared.ClientID,
			Flow:             relay.FlowVision,
			ExitMode:         relay.ExitModeDirect,
			MaxSessions:      cfg.MaxSessions,
			MaxMbps:          cfg.MaxMbps,
			Label:            cfg.Label,
			RelayVersion:     version,
			// A current relay always understands the stream-type discriminator;
			// PunchCapable additionally asks the hub to advertise a direct path.
			StreamTyping: true,
			PunchCapable: cfg.Punch,
		},
		TargetHost: loopHost,
		TargetPort: loopPort,
		OnRegistered: func(ack tunnel.HelloAckFrame) {
			slog.Info("relay published via hub", "public", net.JoinHostPort(ack.PublicHost, strconv.Itoa(ack.PublicPort)), "relay_id", ack.RelayID)
		},
	}
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(ctx) }()
	slog.Info("connecting to relay hub", "hub", cfg.HubAddr, "tls", cfg.HubTLS, "label", cfg.Label)

	select {
	case <-ctx.Done():
		if xrayCmd != nil {
			stopProcess(xrayCmd, xrayErr)
		}
		<-clientDone
		return nil
	case err := <-xrayErr:
		cancel()
		<-clientDone
		if err != nil {
			return fmt.Errorf("xray exited: %w", err)
		}
		return fmt.Errorf("xray exited")
	case err := <-clientDone:
		if xrayCmd != nil {
			stopProcess(xrayCmd, xrayErr)
		}
		if err != nil {
			return fmt.Errorf("tunnel client stopped: %w", err)
		}
		return nil
	}
}

// hubTLSConfig builds the TLS config used to dial the relay hub, or nil for a
// plaintext dial (local development against a non-TLS hub).
func hubTLSConfig(cfg cliConfig) *tls.Config {
	if !cfg.HubTLS {
		return nil
	}
	host, _, err := net.SplitHostPort(cfg.HubAddr)
	if err != nil {
		host = cfg.HubAddr
	}
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: cfg.HubInsecure, //nolint:gosec // gated behind the -hub-insecure flag for testing only
		MinVersion:         tls.VersionTLS12,
	}
}

func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func heartbeatOrRegister(ctx context.Context, broker *volunteer.BrokerClient, cfg cliConfig, prepared preparedRuntime, desc relay.Descriptor) (relay.Descriptor, bool, error) {
	if err := heartbeat(ctx, broker, desc.ID); err != nil {
		if !volunteer.IsRelayNotFound(err) {
			return desc, false, err
		}

		updatedDesc, registerErr := register(ctx, broker, cfg, prepared)
		if registerErr != nil {
			return desc, false, fmt.Errorf("re-register relay after broker forgot %s: %w", desc.ID, registerErr)
		}
		return updatedDesc, true, nil
	}

	return desc, false, nil
}

func isDualListenHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSpace(host))
	return normalized == "dual" || normalized == "both"
}

type preparedRuntime struct {
	ClientID         string
	RealityPublicKey string
	ShortID          string
	XrayConfig       []byte
}

func prepareRuntime(cfg cliConfig) (preparedRuntime, error) {
	clientID := cfg.ClientID
	if clientID == "" {
		generated, err := volunteer.GenerateUUID()
		if err != nil {
			return preparedRuntime{}, fmt.Errorf("generate client ID: %w", err)
		}
		clientID = generated
	}

	shortID := cfg.ShortID
	if shortID == "" {
		generated, err := volunteer.GenerateShortID()
		if err != nil {
			return preparedRuntime{}, fmt.Errorf("generate short ID: %w", err)
		}
		shortID = generated
	}

	privateKey := cfg.RealityPrivateKey
	publicKey := cfg.RealityPublicKey
	if privateKey == "" || publicKey == "" {
		keyPair, err := volunteer.GenerateRealityKeyPair(cfg.XrayPath)
		if err != nil {
			return preparedRuntime{}, err
		}
		privateKey = keyPair.PrivateKey
		publicKey = keyPair.PublicKey
	}

	xrayConfig, err := volunteer.BuildXrayConfig(volunteer.XrayConfigInput{
		ListenHost:        cfg.ListenHost,
		ListenPort:        cfg.ListenPort,
		ClientID:          clientID,
		Flow:              relay.FlowVision,
		Dest:              cfg.RealityDest,
		ServerName:        cfg.ServerName,
		RealityPrivateKey: privateKey,
		ShortID:           shortID,
	})
	if err != nil {
		return preparedRuntime{}, err
	}

	return preparedRuntime{
		ClientID:         clientID,
		RealityPublicKey: publicKey,
		ShortID:          shortID,
		XrayConfig:       xrayConfig,
	}, nil
}

func register(ctx context.Context, broker *volunteer.BrokerClient, cfg cliConfig, prepared preparedRuntime) (relay.Descriptor, error) {
	// The foundation token's cleartext-transport guard lives in BrokerClient
	// (RequireSecureTransport, set by brokerClient() for foundation), so it
	// covers heartbeat as well as registration and also refuses redirects.
	req := relay.RegisterRequest{
		PublicHost:       cfg.PublicHost,
		PublicPort:       cfg.PublicPort,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         prepared.ClientID,
		RealityPublicKey: prepared.RealityPublicKey,
		ShortID:          prepared.ShortID,
		ServerName:       cfg.ServerName,
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		MaxSessions:      cfg.MaxSessions,
		MaxMbps:          cfg.MaxMbps,
		RelayVersion:     version,
		Label:            cfg.Label,
		NodeClass:        cfg.NodeClass,
	}
	desc, err := broker.Register(ctx, req)
	if err != nil {
		return relay.Descriptor{}, err
	}
	// A broker that predates node_class drops the field and answers 201 with
	// no class attested; without this check the relay would silently serve
	// mislabeled as a volunteer-class relay.
	if req.NodeClass == relay.NodeClassFoundation && desc.NodeClass != relay.NodeClassFoundation {
		return relay.Descriptor{}, fmt.Errorf("broker attested node_class %q instead of %q: the broker likely predates node_class support; upgrade it, or drop -node-class to serve as a volunteer-class relay", desc.NodeClass, relay.NodeClassFoundation)
	}
	return desc, nil
}

func heartbeat(ctx context.Context, broker *volunteer.BrokerClient, id string) error {
	return broker.Heartbeat(ctx, id)
}

func (c cliConfig) brokerClient() *volunteer.BrokerClient {
	// A foundation token is the bearer and, on its own, requires secure
	// transport (https + redirect refusal), so the guarantee holds even if the
	// posture normalization above has not run for this config.
	token := c.RegistrationToken
	if c.FoundationToken != "" {
		token = c.FoundationToken
	}
	return &volunteer.BrokerClient{
		BaseURL:                c.BrokerURL,
		Token:                  token,
		HTTPClient:             c.HTTPClient,
		RequireSecureTransport: c.FoundationToken != "" || c.NodeClass == relay.NodeClassFoundation,
	}
}

func stopProcess(cmd *exec.Cmd, errCh <-chan error) {
	if cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(os.Interrupt)

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
