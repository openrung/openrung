package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/punch"
	"openrung/internal/relay"
)

// defaultPunchPort is the hub punch coordinator port assumed when -punch-url is
// not given (the relay's public host is the hub for tunnel relays).
const defaultPunchPort = "9444"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "check":
		return runCheck(args[1:])
	case "config":
		return runConfig(args[1:])
	case "connect":
		return runConnect(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return usageError()
	}
}

func runCheck(args []string) error {
	cfg, err := parseCommonFlags("check", args)
	if err != nil {
		return err
	}

	selected, _, _, err := fetchSelectedRelay(context.Background(), cfg, "", "")
	if err != nil {
		return err
	}
	printSelectedRelay(os.Stdout, selected)
	return nil
}

func runConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfg := commonConfig{}
	addCommonFlags(fs, &cfg)
	outPath := fs.String("out", "", "write generated sing-box config to this path; defaults to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selected, configJSON, _, err := fetchSelectedRelay(context.Background(), cfg, "", "")
	if err != nil {
		return err
	}

	if *outPath == "" || *outPath == "-" {
		fmt.Print(string(configJSON))
		return nil
	}
	if err := os.WriteFile(*outPath, configJSON, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(os.Stdout, "wrote sing-box config for relay %s to %s\n", selected.ID, *outPath)
	return nil
}

func runConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	cfg := commonConfig{}
	addCommonFlags(fs, &cfg)
	singBoxPath := fs.String("sing-box", "sing-box", "path to sing-box binary")
	configOut := fs.String("config-out", "", "optional path for generated sing-box config")
	punchEnabled := fs.Bool("punch", true, "attempt a direct NAT-punched path before falling back to the relay")
	punchURL := fs.String("punch-url", "", "override the hub punch coordinator base URL (else use the relay's advertised punch_endpoint)")
	punchInsecure := fs.Bool("punch-insecure", false, "skip TLS verification of the hub punch API (for a self-signed hub cert; testing)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	mgr := newConnectManager(cfg.BrokerURL)
	session, _ := mgr.BeginSession()
	clientID, sessionID := sessionIdentity(session)
	// Resolve public-IP geo concurrently (don't block connect on it) so the
	// broker dashboard can attribute country/city/ISP to the real client, the
	// way the mobile apps do. Heartbeats and later events pick it up once ready.
	go mgr.SetGeoAttributes(clienttelemetry.LookupGeoAttributes(ctx, nil))
	mgr.Record("connection_attempted", "", nil, nil)

	selected, configJSON, brokerFetch, err := fetchSelectedRelay(ctx, cfg, clientID, sessionID)
	if err != nil {
		recordConnectFailure(ctx, mgr, "broker_fetch", "", err)
		return err
	}

	// Try a direct NAT-punched path first; on any failure fall back to the relay
	// endpoint so the outcome is never worse than today.
	connectHost, connectPort := selected.PublicHost, selected.PublicPort
	if est, punchedConfig := maybePunch(ctx, cfg, mgr, selected, *punchEnabled, *punchURL, *punchInsecure); est != nil {
		defer est.Close()
		go func() { _ = est.Bridge.Serve(ctx) }()
		configJSON = punchedConfig
		connectHost, connectPort = est.BridgeHost, est.BridgePort
		fmt.Fprintf(os.Stdout, "punched direct path to relay %s (peer %s, nat %s)\n", selected.ID, est.PeerIP, est.NATClass)
	}

	relayTCPMs, err := tcpReachMs(ctx, connectHost, connectPort)
	if err != nil {
		mgr.Record("relay_attempt_failed", selected.ID,
			map[string]string{"error_type": errorType(err)},
			map[string]int64{"attempt": 1})
		recordConnectFailure(ctx, mgr, "relay_connect", selected.ID, err)
		return fmt.Errorf("relay %s unreachable at %s:%d: %w", selected.ID, connectHost, connectPort, err)
	}

	configPath, cleanup, err := writeConnectConfig(*configOut, configJSON)
	if err != nil {
		recordConnectFailure(ctx, mgr, "config_write", selected.ID, err)
		return err
	}
	defer cleanup()

	printSelectedRelay(os.Stdout, selected)
	fmt.Fprintf(os.Stdout, "starting sing-box with config %s\n", configPath)

	mgr.MarkConnected(selected.ID)
	runner := client.SingBoxRunner{
		Path:   *singBoxPath,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	tunnelStarted := time.Now()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- runner.Run(ctx, configPath) }()

	probeMs, probeOK := probeInternet(ctx, cfg.BrokerURL)
	tunnelStartMs := time.Since(tunnelStarted).Milliseconds()

	// If sing-box already exited, the tunnel never came up.
	select {
	case runErr := <-runErrCh:
		recordConnectFailure(ctx, mgr, "tunnel_start", selected.ID, runErr)
		return runErr
	default:
	}

	measurements := map[string]int64{
		"broker_fetch_ms": brokerFetch.Milliseconds(),
		"relay_tcp_ms":    relayTCPMs,
		"tunnel_start_ms": tunnelStartMs,
		"relay_attempts":  1,
	}
	if probeOK {
		measurements["internet_probe_ms"] = probeMs
	}
	mgr.Record("connection_succeeded", selected.ID, nil, measurements)
	if err := mgr.Flush(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry flush failed: %v\n", err)
	}

	go mgr.RunHeartbeatLoop(ctx)

	runErr := <-runErrCh
	reason := "tunnel_exited"
	if ctx.Err() != nil {
		reason = "disconnect"
	}
	mgr.Record("tunnel_stopped", selected.ID, nil, nil)
	mgr.EndSession(reason)
	flushOnShutdown(mgr)
	return runErr
}

// sessionIdentity returns the client and session ids for a (possibly nil)
// session, used to populate identity headers on the relay-list request.
func sessionIdentity(session *clienttelemetry.Session) (clientID, sessionID string) {
	if session == nil {
		return "", ""
	}
	return session.ClientID, session.ID
}

// recordConnectFailure emits connection_failed, ends the session, and flushes.
func recordConnectFailure(ctx context.Context, mgr *clienttelemetry.Manager, stage, relayID string, err error) {
	mgr.Record("connection_failed", relayID, map[string]string{
		"failure_stage": stage,
		"error_type":    errorType(err),
	}, nil)
	mgr.EndSession("connection_failed")
	flushOnShutdown(mgr)
}

// flushOnShutdown flushes remaining telemetry using a fresh, bounded context so
// it still runs when the connect context has already been cancelled.
func flushOnShutdown(mgr *clienttelemetry.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Flush(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry flush failed: %v\n", err)
	}
}

type commonConfig struct {
	BrokerURL  string
	Limit      int
	MTU        int
	Family     string
	RelayID    string
	RelayLabel string
}

func parseCommonFlags(name string, args []string) (commonConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfg := commonConfig{}
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return commonConfig{}, err
	}
	return cfg, nil
}

func addCommonFlags(fs *flag.FlagSet, cfg *commonConfig) {
	fs.StringVar(&cfg.BrokerURL, "broker", "http://localhost:8080", "broker base URL")
	fs.IntVar(&cfg.Limit, "limit", 5, "relay candidate limit")
	fs.IntVar(&cfg.MTU, "mtu", 0, "TUN MTU; defaults to sing-box config default")
	fs.StringVar(&cfg.Family, "relay-family", string(client.RelayFamilyAuto), "relay address family: auto, ipv4, or ipv6")
	fs.StringVar(&cfg.RelayID, "relay-id", "", "connect only to the relay with this exact broker relay id (e.g. relay_abc...)")
	fs.StringVar(&cfg.RelayLabel, "relay-label", "", "connect only to the relay(s) with this label")
}

// fetchSelectedRelay fetches relay candidates and selects one. clientID and
// sessionID, when set, are forwarded as identity headers so the broker records a
// client_seen event. The returned duration is the time spent in the broker
// relay-list call (broker_fetch_ms for telemetry).
func fetchSelectedRelay(ctx context.Context, cfg commonConfig, clientID, sessionID string) (relay.Descriptor, []byte, time.Duration, error) {
	broker := client.BrokerClient{BaseURL: cfg.BrokerURL}
	// When pinning a specific relay, fetch the full candidate set so the target
	// isn't ranked out of a small -limit window.
	limit := cfg.Limit
	if cfg.RelayID != "" || cfg.RelayLabel != "" {
		limit = 20
	}
	fetchStarted := time.Now()
	resp, err := broker.ListRelays(ctx, limit, clientID, sessionID)
	brokerFetch := time.Since(fetchStarted)
	if err != nil {
		return relay.Descriptor{}, nil, brokerFetch, err
	}

	if cfg.RelayID != "" || cfg.RelayLabel != "" {
		matched := make([]relay.Descriptor, 0, len(resp.Relays))
		for _, r := range resp.Relays {
			if (cfg.RelayID != "" && r.ID == cfg.RelayID) || (cfg.RelayLabel != "" && r.Label == cfg.RelayLabel) {
				matched = append(matched, r)
			}
		}
		if len(matched) == 0 {
			return relay.Descriptor{}, nil, brokerFetch, fmt.Errorf("no relay matched -relay-id=%q / -relay-label=%q among %d candidates", cfg.RelayID, cfg.RelayLabel, len(resp.Relays))
		}
		resp.Relays = matched
	}

	family, err := client.ParseRelayFamily(cfg.Family)
	if err != nil {
		return relay.Descriptor{}, nil, brokerFetch, err
	}

	selected, err := client.SelectRelayForFamily(resp, family)
	if err != nil {
		if errors.Is(err, client.ErrNoUsableRelay) {
			return relay.Descriptor{}, nil, brokerFetch, fmt.Errorf("no usable relay returned by broker")
		}
		return relay.Descriptor{}, nil, brokerFetch, err
	}

	configJSON, err := client.BuildSingBoxConfig(client.SingBoxConfigInput{Relay: selected, MTU: cfg.MTU})
	if err != nil {
		return relay.Descriptor{}, nil, brokerFetch, err
	}
	return selected, configJSON, brokerFetch, nil
}

// maybePunch attempts a direct NAT-punched path to the selected relay. On success
// it returns a live Establishment (whose Bridge the caller must Serve) and a
// sing-box config pointed at the loopback bridge. On any failure — including a
// relay that is not punch-capable or punching disabled — it returns (nil, nil)
// and the caller uses the relay endpoint. All outcomes are recorded as telemetry.
func maybePunch(ctx context.Context, cfg commonConfig, mgr *clienttelemetry.Manager, selected relay.Descriptor, enabled bool, urlOverride string, insecure bool) (*punch.Establishment, []byte) {
	if !enabled || !selected.PunchCapable {
		if selected.PunchCapable && !enabled {
			mgr.Record("punch_skipped", selected.ID, map[string]string{"reason": "disabled"}, nil)
		}
		return nil, nil
	}

	mgr.Record("punch_attempted", selected.ID, nil, nil)
	dialer := &punch.Dialer{
		Hub:     punch.HubClient{BaseURL: punchBaseURL(urlOverride, selected), HTTPClient: punchHTTPClient(insecure)},
		RelayID: selected.ID,
	}
	est, res, err := dialer.Establish(ctx)
	if err != nil {
		mgr.Record("punch_failed", selected.ID,
			map[string]string{"reason": res.Reason, "nat_class": res.NATClass}, nil)
		return nil, nil
	}

	punchedConfig, cErr := client.BuildSingBoxConfig(client.SingBoxConfigInput{
		Relay:                   selected,
		MTU:                     cfg.MTU,
		BridgeHost:              est.BridgeHost,
		BridgePort:              est.BridgePort,
		PunchPeerExcludeAddress: est.PeerIP,
	})
	if cErr != nil {
		_ = est.Close()
		mgr.Record("punch_failed", selected.ID, map[string]string{"reason": "config"}, nil)
		return nil, nil
	}

	mgr.Record("punch_succeeded", selected.ID,
		map[string]string{"nat_class": res.NATClass},
		map[string]int64{"punch_rtt_ms": res.RTTMillis})
	return est, punchedConfig
}

// punchBaseURL resolves the hub punch coordinator base URL: an explicit override
// wins, then the relay's advertised punch_endpoint (correct scheme/host/port),
// then a legacy http://<relay-public-host>:9444 fallback.
func punchBaseURL(override string, selected relay.Descriptor) string {
	if override != "" {
		return override
	}
	if selected.PunchEndpoint != "" {
		return selected.PunchEndpoint
	}
	return "http://" + net.JoinHostPort(selected.PublicHost, defaultPunchPort)
}

// punchHTTPClient returns the HTTP client for the hub punch API. With insecure
// set it skips TLS verification, for a hub serving a self-signed cert on its
// HTTPS punch endpoint. The punched QUIC path is unaffected (it pins the
// volunteer's per-session cert by fingerprint regardless).
func punchHTTPClient(insecure bool) *http.Client {
	if !insecure {
		return nil // punch.HubClient uses its default client
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // gated behind -punch-insecure for a self-signed hub cert
		},
	}
}

func writeConnectConfig(configOut string, configJSON []byte) (string, func(), error) {
	if configOut != "" {
		if err := os.WriteFile(configOut, configJSON, 0o600); err != nil {
			return "", func() {}, fmt.Errorf("write config: %w", err)
		}
		return configOut, func() {}, nil
	}

	file, err := os.CreateTemp("", "openrung-sing-box-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp config: %w", err)
	}
	path := file.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}

	if _, err := file.Write(configJSON); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write temp config: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close temp config: %w", err)
	}
	return path, cleanup, nil
}

func printSelectedRelay(out *os.File, selected relay.Descriptor) {
	expires := selected.ExpiresAt.Format(time.RFC3339)
	fmt.Fprintf(
		out,
		"selected relay %s at %s:%d, expires %s\n",
		selected.ID,
		selected.PublicHost,
		selected.PublicPort,
		expires,
	)
}

func usageError() error {
	printUsage()
	return fmt.Errorf("expected one of: check, config, connect")
}

func printUsage() {
	program := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `Usage:
  %[1]s check   -broker http://localhost:8080
  %[1]s config  -broker http://localhost:8080 -out openrung-sing-box.json
  %[1]s connect -broker http://localhost:8080 -sing-box /opt/homebrew/bin/sing-box

Commands:
  check    Fetch relay candidates and print the selected usable relay.
  config   Generate a sing-box TUN client config for the selected relay.
  connect  Generate a config and run sing-box to route traffic through OpenRung.

Common flags:
  -mtu            Override the generated TUN MTU, e.g. -mtu 1280 for IPv6 path tests.
  -relay-family   Select relay family: auto, ipv4, or ipv6.

`, program)
}
