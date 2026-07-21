package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openrung/internal/wssbridge"
)

//go:embed VERSION
var baseVersion string

var (
	version  string
	revision = "unknown"
)

var errVersionRequested = errors.New("version requested")

const (
	defaultAddr             = "127.0.0.1:8081"
	defaultReplayEntries    = 100_000
	defaultReplayStatePath  = "/var/lib/openrung/wss-replay.journal"
	defaultClockSkew        = 30 * time.Second
	defaultStatsLogInterval = time.Minute
)

type config struct {
	Addr                 string
	RelayID              string
	FrontOriginTokens    map[string][]string
	ViewerAddressHeader  string
	TicketPublicKeys     string
	FixedTarget          string
	MaxSessions          int
	MaxPendingHandshakes int
	MaxStreamsPerSession int
	MaxGlobalStreams     int
	MaxSessionsPerSource int
	MaxStreamsPerSource  int
	GlobalHandshakeRate  float64
	GlobalHandshakeBurst int
	HandshakeRate        float64
	HandshakeBurst       int
	MaxTrackedSources    int
	ReplayEntries        int
	ReplayStatePath      string
	ClockSkew            time.Duration
	HandshakeTimeout     time.Duration
	DialTimeout          time.Duration
	FirstByteTimeout     time.Duration
	StreamIdleTimeout    time.Duration
	SessionLifetime      time.Duration
	NoStreamIdleTimeout  time.Duration
	PingInterval         time.Duration
	PingWriteTimeout     time.Duration
	StatsLogInterval     time.Duration
}

func main() {
	if err := run(); err != nil {
		slog.Error("WSS sidecar stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, errVersionRequested) {
		fmt.Println(versionInfo())
		return nil
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, slog.Default())
}

func serve(ctx context.Context, cfg config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	keys, err := wssbridge.ParseTicketPublicKeys(cfg.TicketPublicKeys)
	if err != nil {
		return fmt.Errorf("parse ticket public keys: %w", err)
	}
	verifier, err := wssbridge.NewTicketVerifier(keys, cfg.RelayID, wssbridge.TicketOptions{ClockSkew: cfg.ClockSkew})
	if err != nil {
		return fmt.Errorf("configure ticket verifier: %w", err)
	}
	stats := &wssbridge.SidecarStats{}
	replayStore, err := wssbridge.OpenDurableReplayStore(cfg.ReplayStatePath, cfg.ReplayEntries)
	if err != nil {
		return fmt.Errorf("open durable replay state: %w", err)
	}
	defer replayStore.Close()
	handler, err := wssbridge.NewSidecarHandler(wssbridge.SidecarOptions{
		RelayID: cfg.RelayID, FrontOriginTokens: cfg.FrontOriginTokens,
		ViewerAddressHeader: cfg.ViewerAddressHeader, Verifier: verifier,
		ReplayStore: replayStore, Stats: stats,
		FixedTarget: cfg.FixedTarget,
		MaxSessions: cfg.MaxSessions, MaxPendingHandshakes: cfg.MaxPendingHandshakes,
		MaxStreamsPerSession: cfg.MaxStreamsPerSession,
		MaxGlobalStreams:     cfg.MaxGlobalStreams, MaxSessionsPerSource: cfg.MaxSessionsPerSource,
		MaxStreamsPerSource:          cfg.MaxStreamsPerSource,
		GlobalHandshakeRatePerSecond: cfg.GlobalHandshakeRate,
		GlobalHandshakeBurst:         cfg.GlobalHandshakeBurst,
		HandshakeRatePerSecond:       cfg.HandshakeRate, HandshakeBurst: cfg.HandshakeBurst,
		MaxTrackedSources: cfg.MaxTrackedSources, HandshakeTimeout: cfg.HandshakeTimeout,
		DialTimeout: cfg.DialTimeout, FirstByteTimeout: cfg.FirstByteTimeout,
		StreamIdleTimeout: cfg.StreamIdleTimeout, SessionLifetime: cfg.SessionLifetime,
		NoStreamIdleTimeout: cfg.NoStreamIdleTimeout, PingInterval: cfg.PingInterval,
		PingWriteTimeout: cfg.PingWriteTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure WSS sidecar: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return errors.New("listen for WSS sidecar failed")
	}
	defer listener.Close()

	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: cfg.HandshakeTimeout,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	logger.Info("starting WSS sidecar",
		"version", resolvedVersion(), "revision", resolvedRevision(),
		"max_sessions", cfg.MaxSessions,
		"max_pending_handshakes", cfg.MaxPendingHandshakes,
		"max_global_streams", cfg.MaxGlobalStreams,
		"max_sessions_per_source", cfg.MaxSessionsPerSource,
		"max_streams_per_source", cfg.MaxStreamsPerSource,
		"global_handshake_rate", cfg.GlobalHandshakeRate,
		"global_handshake_burst", cfg.GlobalHandshakeBurst,
	)
	go logSidecarStats(ctx, logger, stats, cfg.StatsLogInterval)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("shut down WSS sidecar failed")
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// logSidecarStats emits only aggregate values with fixed names and no labels.
func logSidecarStats(ctx context.Context, logger *slog.Logger, stats *wssbridge.SidecarStats, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := stats.Snapshot()
			logger.Info("WSS sidecar aggregate",
				"accepted_sessions", s.AcceptedSessions, "current_sessions", s.CurrentSessions,
				"accepted_streams", s.AcceptedStreams, "current_streams", s.CurrentStreams,
				"origin_auth_rejections", s.OriginAuthRejections,
				"viewer_address_rejections", s.ViewerAddressRejections,
				"protocol_rejections", s.ProtocolRejections,
				"global_handshake_rate_rejections", s.GlobalHandshakeRateRejections,
				"handshake_rate_rejections", s.HandshakeRateRejections,
				"handshake_concurrency_rejections", s.HandshakeConcurrencyRejections,
				"handshake_limiter_fail_open", s.HandshakeLimiterFailOpen,
				"ticket_rejections", s.TicketRejections, "front_rejections", s.FrontRejections,
				"replay_rejections", s.ReplayRejections, "replay_store_failures", s.ReplayStoreFailures,
				"session_limit_rejections", s.SessionLimitRejections,
				"source_session_rejections", s.SourceSessionRejections,
				"stream_limit_rejections", s.StreamLimitRejections,
				"source_stream_rejections", s.SourceStreamRejections,
				"upgrade_failures", s.UpgradeFailures, "dial_failures", s.DialFailures,
				"idle_session_closes", s.IdleSessionCloses,
				"lifetime_session_closes", s.LifetimeSessionCloses,
			)
		}
	}
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	envOr := func(key, fallback string) string {
		if value := getenv(key); value != "" {
			return value
		}
		return fallback
	}
	var cfg config
	var showVersion bool
	frontTokensJSON := getenv("OPENRUNG_WSS_FRONT_ORIGIN_TOKENS")
	values := map[string]string{
		"max-sessions":            envOr("OPENRUNG_WSS_MAX_SESSIONS", strconv.Itoa(wssbridge.DefaultMaxSessions)),
		"max-pending-handshakes":  envOr("OPENRUNG_WSS_MAX_PENDING_HANDSHAKES", strconv.Itoa(wssbridge.DefaultMaxPendingHandshakes)),
		"max-streams-per-session": envOr("OPENRUNG_WSS_MAX_STREAMS_PER_SESSION", strconv.Itoa(wssbridge.DefaultMaxStreamsPerSession)),
		"max-global-streams":      envOr("OPENRUNG_WSS_MAX_GLOBAL_STREAMS", strconv.Itoa(wssbridge.DefaultMaxGlobalStreams)),
		"max-sessions-per-source": envOr("OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE", strconv.Itoa(wssbridge.DefaultMaxSessionsPerSource)),
		"max-streams-per-source":  envOr("OPENRUNG_WSS_MAX_STREAMS_PER_SOURCE", strconv.Itoa(wssbridge.DefaultMaxStreamsPerSource)),
		"global-handshake-rate":   envOr("OPENRUNG_WSS_GLOBAL_HANDSHAKE_RATE", strconv.FormatFloat(wssbridge.DefaultGlobalHandshakeRatePerSecond, 'f', -1, 64)),
		"global-handshake-burst":  envOr("OPENRUNG_WSS_GLOBAL_HANDSHAKE_BURST", strconv.Itoa(wssbridge.DefaultGlobalHandshakeBurst)),
		"handshake-rate":          envOr("OPENRUNG_WSS_HANDSHAKE_RATE", strconv.FormatFloat(wssbridge.DefaultHandshakeRatePerSecond, 'f', -1, 64)),
		"handshake-burst":         envOr("OPENRUNG_WSS_HANDSHAKE_BURST", strconv.Itoa(wssbridge.DefaultHandshakeBurst)),
		"max-tracked-sources":     envOr("OPENRUNG_WSS_MAX_TRACKED_SOURCES", strconv.Itoa(wssbridge.DefaultMaxTrackedSources)),
		"replay-entries":          envOr("OPENRUNG_WSS_REPLAY_ENTRIES", strconv.Itoa(defaultReplayEntries)),
	}
	durations := map[string]string{
		"clock-skew":             envOr("OPENRUNG_WSS_CLOCK_SKEW", defaultClockSkew.String()),
		"handshake-timeout":      envOr("OPENRUNG_WSS_HANDSHAKE_TIMEOUT", (10 * time.Second).String()),
		"dial-timeout":           envOr("OPENRUNG_WSS_DIAL_TIMEOUT", wssbridge.DefaultDialTimeout.String()),
		"first-byte-timeout":     envOr("OPENRUNG_WSS_FIRST_BYTE_TIMEOUT", wssbridge.DefaultFirstByteTimeout.String()),
		"stream-idle-timeout":    envOr("OPENRUNG_WSS_STREAM_IDLE_TIMEOUT", wssbridge.DefaultStreamIdleTimeout.String()),
		"session-lifetime":       envOr("OPENRUNG_WSS_SESSION_LIFETIME", wssbridge.DefaultSessionLifetime.String()),
		"no-stream-idle-timeout": envOr("OPENRUNG_WSS_NO_STREAM_IDLE_TIMEOUT", wssbridge.DefaultNoStreamIdleTimeout.String()),
		"ping-interval":          envOr("OPENRUNG_WSS_PING_INTERVAL", wssbridge.DefaultSidecarPingInterval.String()),
		"ping-write-timeout":     envOr("OPENRUNG_WSS_PING_WRITE_TIMEOUT", wssbridge.DefaultSidecarPingTimeout.String()),
		"stats-log-interval":     envOr("OPENRUNG_WSS_STATS_LOG_INTERVAL", defaultStatsLogInterval.String()),
	}

	fs := flag.NewFlagSet("wsssidecar", flag.ContinueOnError)
	fs.BoolVar(&showVersion, "version", false, "print WSS sidecar version and exit")
	fs.StringVar(&cfg.Addr, "addr", envOr("OPENRUNG_WSS_ADDR", defaultAddr), "loopback HTTP listener behind origin TLS")
	fs.StringVar(&cfg.RelayID, "relay-id", getenv("OPENRUNG_WSS_RELAY_ID"), "exact local relay ID")
	fs.StringVar(&frontTokensJSON, "front-origin-tokens", frontTokensJSON, "JSON object mapping front IDs to overlapping origin tokens")
	fs.StringVar(&cfg.ViewerAddressHeader, "viewer-address-header", envOr("OPENRUNG_WSS_VIEWER_ADDRESS_HEADER", wssbridge.DefaultViewerAddressHeader), "trusted CDN viewer-address header")
	fs.StringVar(&cfg.TicketPublicKeys, "ticket-public-keys", getenv("OPENRUNG_WSS_TICKET_PUBLIC_KEYS"), "comma-separated broker ticket public keys")
	fs.StringVar(&cfg.FixedTarget, "fixed-target", envOr("OPENRUNG_WSS_FIXED_TARGET", wssbridge.DefaultFixedTarget), "fixed loopback Reality endpoint")
	fs.StringVar(&cfg.ReplayStatePath, "replay-state", envOr("OPENRUNG_WSS_REPLAY_STATE", defaultReplayStatePath), "absolute durable replay journal path")
	maxSessions := values["max-sessions"]
	maxPendingHandshakes := values["max-pending-handshakes"]
	maxPerSession := values["max-streams-per-session"]
	maxGlobal := values["max-global-streams"]
	maxSourceSessions := values["max-sessions-per-source"]
	maxSourceStreams := values["max-streams-per-source"]
	globalHandshakeRate := values["global-handshake-rate"]
	globalHandshakeBurst := values["global-handshake-burst"]
	handshakeRate := values["handshake-rate"]
	handshakeBurst := values["handshake-burst"]
	maxTracked := values["max-tracked-sources"]
	replayEntries := values["replay-entries"]
	clockSkew := durations["clock-skew"]
	handshakeTimeout := durations["handshake-timeout"]
	dialTimeout := durations["dial-timeout"]
	firstByteTimeout := durations["first-byte-timeout"]
	streamIdleTimeout := durations["stream-idle-timeout"]
	sessionLifetime := durations["session-lifetime"]
	noStreamIdleTimeout := durations["no-stream-idle-timeout"]
	pingInterval := durations["ping-interval"]
	pingWriteTimeout := durations["ping-write-timeout"]
	statsLogInterval := durations["stats-log-interval"]
	fs.StringVar(&maxSessions, "max-sessions", maxSessions, "maximum concurrent WebSocket sessions")
	fs.StringVar(&maxPendingHandshakes, "max-pending-handshakes", maxPendingHandshakes, "maximum in-flight ticket/replay/upgrade handshakes")
	fs.StringVar(&maxPerSession, "max-streams-per-session", maxPerSession, "maximum streams per session")
	fs.StringVar(&maxGlobal, "max-global-streams", maxGlobal, "maximum concurrent streams")
	fs.StringVar(&maxSourceSessions, "max-sessions-per-source", maxSourceSessions, "maximum sessions per trusted source")
	fs.StringVar(&maxSourceStreams, "max-streams-per-source", maxSourceStreams, "maximum streams per trusted source")
	fs.StringVar(&globalHandshakeRate, "global-handshake-rate", globalHandshakeRate, "global handshake tokens per second")
	fs.StringVar(&globalHandshakeBurst, "global-handshake-burst", globalHandshakeBurst, "global handshake burst")
	fs.StringVar(&handshakeRate, "handshake-rate", handshakeRate, "per-source handshake tokens per second")
	fs.StringVar(&handshakeBurst, "handshake-burst", handshakeBurst, "per-source handshake burst")
	fs.StringVar(&maxTracked, "max-tracked-sources", maxTracked, "maximum tracked handshake sources")
	fs.StringVar(&replayEntries, "replay-entries", replayEntries, "maximum live replay entries")
	fs.StringVar(&clockSkew, "clock-skew", clockSkew, "ticket clock skew")
	fs.StringVar(&handshakeTimeout, "handshake-timeout", handshakeTimeout, "WebSocket handshake timeout")
	fs.StringVar(&dialTimeout, "dial-timeout", dialTimeout, "fixed loopback dial timeout")
	fs.StringVar(&firstByteTimeout, "first-byte-timeout", firstByteTimeout, "first stream byte timeout")
	fs.StringVar(&streamIdleTimeout, "stream-idle-timeout", streamIdleTimeout, "active stream idle timeout")
	fs.StringVar(&sessionLifetime, "session-lifetime", sessionLifetime, "maximum session lifetime")
	fs.StringVar(&noStreamIdleTimeout, "no-stream-idle-timeout", noStreamIdleTimeout, "between-stream idle timeout")
	fs.StringVar(&pingInterval, "ping-interval", pingInterval, "WebSocket ping interval")
	fs.StringVar(&pingWriteTimeout, "ping-write-timeout", pingWriteTimeout, "WebSocket ping write timeout")
	fs.StringVar(&statsLogInterval, "stats-log-interval", statsLogInterval, "aggregate stats log interval")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if showVersion {
		return config{}, errVersionRequested
	}
	if fs.NArg() != 0 {
		return config{}, errors.New("unexpected positional arguments")
	}
	if err := json.Unmarshal([]byte(frontTokensJSON), &cfg.FrontOriginTokens); err != nil {
		return config{}, errors.New("front-origin-tokens must be a JSON object of string arrays")
	}
	var err error
	if cfg.MaxSessions, err = parsePositiveInt("max-sessions", maxSessions); err != nil {
		return config{}, err
	}
	if cfg.MaxPendingHandshakes, err = parsePositiveInt("max-pending-handshakes", maxPendingHandshakes); err != nil {
		return config{}, err
	}
	if cfg.MaxStreamsPerSession, err = parsePositiveInt("max-streams-per-session", maxPerSession); err != nil {
		return config{}, err
	}
	if cfg.MaxGlobalStreams, err = parsePositiveInt("max-global-streams", maxGlobal); err != nil {
		return config{}, err
	}
	if cfg.MaxSessionsPerSource, err = parsePositiveInt("max-sessions-per-source", maxSourceSessions); err != nil {
		return config{}, err
	}
	if cfg.MaxStreamsPerSource, err = parsePositiveInt("max-streams-per-source", maxSourceStreams); err != nil {
		return config{}, err
	}
	if cfg.GlobalHandshakeRate, err = parsePositiveFloat("global-handshake-rate", globalHandshakeRate); err != nil {
		return config{}, err
	}
	if cfg.GlobalHandshakeBurst, err = parsePositiveInt("global-handshake-burst", globalHandshakeBurst); err != nil {
		return config{}, err
	}
	if cfg.HandshakeRate, err = parsePositiveFloat("handshake-rate", handshakeRate); err != nil {
		return config{}, err
	}
	if cfg.HandshakeBurst, err = parsePositiveInt("handshake-burst", handshakeBurst); err != nil {
		return config{}, err
	}
	if cfg.MaxTrackedSources, err = parsePositiveInt("max-tracked-sources", maxTracked); err != nil {
		return config{}, err
	}
	if cfg.ReplayEntries, err = parsePositiveInt("replay-entries", replayEntries); err != nil {
		return config{}, err
	}
	if cfg.ClockSkew, err = parseNonNegativeDuration("clock-skew", clockSkew); err != nil {
		return config{}, err
	}
	if cfg.HandshakeTimeout, err = parsePositiveDuration("handshake-timeout", handshakeTimeout); err != nil {
		return config{}, err
	}
	if cfg.DialTimeout, err = parsePositiveDuration("dial-timeout", dialTimeout); err != nil {
		return config{}, err
	}
	if cfg.FirstByteTimeout, err = parsePositiveDuration("first-byte-timeout", firstByteTimeout); err != nil {
		return config{}, err
	}
	if cfg.StreamIdleTimeout, err = parsePositiveDuration("stream-idle-timeout", streamIdleTimeout); err != nil {
		return config{}, err
	}
	if cfg.SessionLifetime, err = parsePositiveDuration("session-lifetime", sessionLifetime); err != nil {
		return config{}, err
	}
	if cfg.NoStreamIdleTimeout, err = parsePositiveDuration("no-stream-idle-timeout", noStreamIdleTimeout); err != nil {
		return config{}, err
	}
	if cfg.PingInterval, err = parsePositiveDuration("ping-interval", pingInterval); err != nil {
		return config{}, err
	}
	if cfg.PingWriteTimeout, err = parsePositiveDuration("ping-write-timeout", pingWriteTimeout); err != nil {
		return config{}, err
	}
	if cfg.StatsLogInterval, err = parseNonNegativeDuration("stats-log-interval", statsLogInterval); err != nil {
		return config{}, err
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) validate() error {
	host, portText, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return errors.New("addr must be a loopback IP literal and port")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return errors.New("addr must be a loopback IP literal with port 1..65535")
	}
	if c.RelayID == "" || c.TicketPublicKeys == "" {
		return errors.New("relay-id and ticket-public-keys are required")
	}
	if len(c.FrontOriginTokens) == 0 {
		return errors.New("at least one front origin-token set is required")
	}
	if _, err := wssbridge.NormalizeLoopbackTarget(c.FixedTarget); err != nil {
		return err
	}
	if c.ReplayEntries < c.MaxSessions {
		return errors.New("replay-entries must be at least max-sessions")
	}
	if c.ReplayEntries > wssbridge.MaxReplayEntries {
		return fmt.Errorf("replay-entries must not exceed %d", wssbridge.MaxReplayEntries)
	}
	if !filepath.IsAbs(c.ReplayStatePath) || filepath.Clean(c.ReplayStatePath) != c.ReplayStatePath {
		return errors.New("replay-state must be a clean absolute file path")
	}
	if c.ClockSkew > wssbridge.MaxTicketClockSkew {
		return fmt.Errorf("clock-skew must not exceed %s", wssbridge.MaxTicketClockSkew)
	}
	if c.MaxStreamsPerSession > wssbridge.MaxTicketStreams {
		return fmt.Errorf("max-streams-per-session must not exceed %d", wssbridge.MaxTicketStreams)
	}
	if c.PingWriteTimeout >= c.PingInterval {
		return errors.New("ping-write-timeout must be shorter than ping-interval")
	}
	return nil
}

func parsePositiveInt(name, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func parsePositiveFloat(name, value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s must be a positive finite number", name)
	}
	return parsed, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func parseNonNegativeDuration(name, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", name)
	}
	return parsed, nil
}

func resolvedVersion() string {
	if value := strings.TrimSpace(version); value != "" {
		return value
	}
	if value := strings.TrimSpace(baseVersion); value != "" {
		return value
	}
	return "dev"
}

func resolvedRevision() string {
	if value := strings.TrimSpace(revision); value != "" {
		return value
	}
	return "unknown"
}

func versionInfo() string {
	return fmt.Sprintf("wsssidecar/%s revision=%s", resolvedVersion(), resolvedRevision())
}
