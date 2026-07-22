package main

import (
	"context"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"openrung/internal/broker"
	"openrung/internal/wssbridge"
)

//go:embed VERSION
var baseVersion string

// version and revision are overridden by release builds using -ldflags.
var (
	version  string
	revision = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("broker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print broker version and exit")
	printWSSTicketPublicKey := flag.Bool("print-wss-ticket-public-key", false, "print the active WSS ticket verification key and exit")
	addr := flag.String("addr", envDefault("OPENRUNG_ADDR", ":8080"), "HTTP listen address")
	leaseTTL := flag.Duration("lease-ttl", 3*time.Minute, "relay lease TTL")
	telemetryFile := flag.String("telemetry-file", envDefault("OPENRUNG_TELEMETRY_FILE", "openrung-telemetry.jsonl"), "append-only client telemetry JSONL file (its directory must be writable)")
	telemetryStore := flag.String("telemetry-store", envDefault("OPENRUNG_TELEMETRY_STORE", "jsonl"), "telemetry storage backend: jsonl or postgres")
	telemetryDatabaseURL := flag.String("telemetry-database-url", envDefault("OPENRUNG_TELEMETRY_DATABASE_URL", os.Getenv("OPENRUNG_RELAY_DATABASE_URL")), "PostgreSQL database URL for telemetry (defaults to the relay database URL)")
	statusInterval := flag.Duration("status-interval", time.Minute, "interval for broker network status logs; 0 disables")
	relayStore := flag.String("relay-store", envDefault("OPENRUNG_RELAY_STORE", "memory"), "relay state backend: memory or postgres")
	relayDatabaseURL := flag.String("relay-database-url", os.Getenv("OPENRUNG_RELAY_DATABASE_URL"), "PostgreSQL database URL for relay state")
	relayRanking := flag.String("relay-ranking", envDefault("OPENRUNG_RELAY_RANKING", "global"), "relay ranking mode: global or legacy")
	geoIPEndpoint := flag.String("geoip-endpoint", envDefault("OPENRUNG_GEOIP_ENDPOINT", broker.DefaultGeoIPEndpoint), "IP geolocation HTTP endpoint for relay city/country lookups (relay host is appended); 'off' disables")
	flag.Parse()
	if *showVersion {
		fmt.Println(versionInfo())
		return nil
	}
	wssTicketSeed, err := parseOptionalWSSTicketSeed(os.Getenv("OPENRUNG_WSS_TICKET_SIGNING_SEED"))
	if err != nil {
		return err
	}
	if *printWSSTicketPublicKey {
		if len(wssTicketSeed) == 0 {
			return errors.New("OPENRUNG_WSS_TICKET_SIGNING_SEED is required")
		}
		publicKey := ed25519.NewKeyFromSeed(wssTicketSeed).Public().(ed25519.PublicKey)
		fmt.Printf("%s=%s\n", wssbridge.TicketKeyID(publicKey), base64.StdEncoding.EncodeToString(publicKey))
		return nil
	}

	rankingMode, err := broker.ParseRankingMode(*relayRanking)
	if err != nil {
		return err
	}

	// Fail closed: with no registration token, anyone can register a relay and
	// poison the directory that clients route their VPN traffic through. Require
	// a token unless the operator explicitly opts into an open broker.
	registrationToken := os.Getenv("OPENRUNG_VOLUNTEER_TOKEN")
	if registrationToken == "" && !envTrue("OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION") {
		return errors.New("OPENRUNG_VOLUNTEER_TOKEN is empty: refusing to start an open broker where anyone can register relays. Set OPENRUNG_VOLUNTEER_TOKEN, or set OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true to run open intentionally")
	}

	// The foundation token authorizes node_class=foundation registrations. It
	// must not equal the (shared, widely distributed) volunteer token, or every
	// volunteer-token holder could label a relay as foundation-operated and
	// clients would trust the signed lie.
	foundationToken := os.Getenv("OPENRUNG_FOUNDATION_TOKEN")
	if foundationToken != "" && foundationToken == registrationToken {
		return errors.New("OPENRUNG_FOUNDATION_TOKEN must differ from OPENRUNG_VOLUNTEER_TOKEN: a shared value would let any holder of the volunteer token register a foundation relay")
	}

	// Fail closed on the signing key too: a missing or malformed seed must
	// crash-loop (an ordinary, visible outage) rather than serve unsigned relay
	// lists, which healthz and old clients would never notice while every
	// verifying client silently lost discovery.
	signingSeed, err := broker.ParseSigningSeed(os.Getenv("OPENRUNG_RELAY_SIGNING_KEY"))
	if err != nil {
		return err
	}
	signingKeyID := broker.SigningKeyID(signingSeed)
	slog.Info("relay list signing enabled", "key_id", signingKeyID)
	geoResolver := newGeoIPResolver(*geoIPEndpoint)
	store, err := newRelayStore(*relayStore, *relayDatabaseURL, rankingMode)
	if err != nil {
		return err
	}
	defer store.Close()

	telemetrySink, err := newTelemetrySink(*telemetryStore, *telemetryFile, *telemetryDatabaseURL)
	if err != nil {
		slog.Error("could not initialize telemetry storage", "error", err)
		return err
	}
	cfg := broker.Config{
		RegistrationToken: registrationToken,
		FoundationToken:   foundationToken,
		RelayLeaseTTL:     *leaseTTL,
		TelemetrySink:     telemetrySink,
		DashboardToken:    os.Getenv("OPENRUNG_DASHBOARD_TOKEN"),
		// Cloudflare's published ranges are trusted by default; add more (e.g. an upstream LB) here.
		TrustedProxyCIDRs:    splitAndTrim(os.Getenv("OPENRUNG_TRUSTED_PROXY_CIDRS")),
		GeoIP:                geoResolver,
		SigningSeed:          signingSeed,
		WSSTicketSigningSeed: wssTicketSeed,
	}
	// The Postgres store aggregates dashboard queries in SQL; the JSONL sink's
	// dashboard is aggregated in Go from its in-memory record set.
	if querier, ok := telemetrySink.(broker.TelemetryQuerier); ok {
		cfg.TelemetryQuerier = querier
	} else {
		cfg.TelemetryReader = telemetrySink
	}
	handler := broker.NewServer(store, cfg)
	maintenanceInterval := *statusInterval
	if maintenanceInterval <= 0 {
		maintenanceInterval = time.Minute
	}
	go maintainBroker(store, telemetrySink, maintenanceInterval, *statusInterval > 0)

	server := &http.Server{
		Addr:    *addr,
		Handler: handler,
		// Bound every phase of a connection so slow-drip (slowloris-style)
		// clients cannot pin goroutines and file descriptors indefinitely. The
		// read window still fits a full 512 KiB telemetry upload on a very slow
		// link; the speed-test handler extends its own write deadline because a
		// 25 MB download legitimately needs longer than the write timeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	shutdownDone := make(chan struct{})
	go func() {
		<-shutdownContext.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("could not gracefully stop broker", "error", err)
			_ = server.Close()
		}
		close(shutdownDone)
	}()

	slog.Info("starting broker", "version", resolvedVersion(), "revision", resolvedRevision(), "addr", *addr, "lease_ttl", leaseTTL.String(), "telemetry_store", *telemetryStore, "telemetry_file", *telemetryFile, "relay_store", *relayStore, "relay_ranking", rankingMode, "dashboard_enabled", os.Getenv("OPENRUNG_DASHBOARD_TOKEN") != "", "foundation_registration_enabled", foundationToken != "", "wss_ticket_issuance_enabled", len(wssTicketSeed) != 0, "status_interval", statusInterval.String(), "geoip_enabled", geoResolver != nil)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		err = nil
	}
	if closeErr := telemetrySink.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
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
	return fmt.Sprintf("broker/%s revision=%s", resolvedVersion(), resolvedRevision())
}

// telemetryStorage is what both telemetry backends provide: the write path,
// the dashboard's read path, and a flush-on-shutdown Close.
type telemetryStorage interface {
	broker.TelemetrySink
	broker.TelemetryReader
	Close() error
}

func newTelemetrySink(storeMode, filePath, databaseURL string) (telemetryStorage, error) {
	switch strings.ToLower(strings.TrimSpace(storeMode)) {
	case "", "jsonl":
		return broker.NewJSONLTelemetrySink(filePath)
	case "postgres":
		if strings.TrimSpace(databaseURL) == "" {
			return nil, errors.New("OPENRUNG_TELEMETRY_STORE=postgres requires OPENRUNG_TELEMETRY_DATABASE_URL (or OPENRUNG_RELAY_DATABASE_URL to share the relay database)")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return broker.NewPostgresTelemetrySink(ctx, databaseURL)
	default:
		return nil, errors.New("telemetry-store must be jsonl or postgres")
	}
}

func newRelayStore(storeMode, databaseURL string, rankingMode broker.RankingMode) (broker.RelayStore, error) {
	switch strings.ToLower(strings.TrimSpace(storeMode)) {
	case "", "memory":
		return broker.NewStoreWithRanking(rankingMode), nil
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return broker.NewPostgresStore(ctx, databaseURL, rankingMode)
	default:
		return nil, errors.New("relay-store must be memory or postgres")
	}
}

// newGeoIPResolver returns nil (geo lookups disabled) for "off"-style values
// so operators can opt out without leaving the flag empty.
func newGeoIPResolver(endpoint string) broker.GeoIPResolver {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "", "off", "disabled", "none":
		return nil
	default:
		return broker.NewHTTPGeoIPResolver(endpoint)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envTrue reports whether an env var is set to a truthy value.
func envTrue(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// splitAndTrim parses a comma-separated env value into a trimmed, non-empty slice.
func splitAndTrim(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseOptionalWSSTicketSeed(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	seed, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("OPENRUNG_WSS_TICKET_SIGNING_SEED must be standard base64")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("OPENRUNG_WSS_TICKET_SIGNING_SEED must decode to %d bytes", ed25519.SeedSize)
	}
	return append([]byte(nil), seed...), nil
}

// pruneTelemetryPartitions drops aged-out telemetry partitions when the sink
// supports it (the Postgres store); the JSONL sink prunes on write, so this is
// a no-op for it. Partial progress is logged even when a later drop fails.
func pruneTelemetryPartitions(telemetry broker.TelemetryReader, now time.Time) {
	pruner, ok := telemetry.(broker.TelemetryPruner)
	if !ok {
		return
	}
	dropped, err := pruner.PruneTelemetry(now)
	for _, name := range dropped {
		slog.Info("telemetry partition dropped", "partition", name)
	}
	if err != nil {
		slog.Error("could not prune telemetry partitions", "error", err)
	}
}

func maintainBroker(store broker.RelayStore, telemetry broker.TelemetryReader, interval time.Duration, logStatus bool) {
	const activityWindow = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for now := range ticker.C {
		now = now.UTC()
		expired, err := store.Prune(now)
		if err != nil {
			slog.Error("could not prune broker state", "error", err)
			continue
		}
		for _, desc := range expired {
			slog.Info("relay expired", "relay_id", desc.ID, "last_heartbeat_at", desc.LastHeartbeatAt)
		}
		pruneTelemetryPartitions(telemetry, now)
		if !logStatus {
			continue
		}

		storeStats, err := store.Stats(now)
		if err != nil {
			slog.Error("could not read broker store stats", "error", err)
			continue
		}
		telemetryStats := broker.BuildOperationalTelemetryStats(
			telemetry.TelemetryRecords(now.Add(-activityWindow)),
			now,
			activityWindow,
		)
		slog.Info(
			"network status",
			"active_relays", storeStats.ActiveRelays,
			"advertised_session_capacity", storeStats.AdvertisedSessionCapacity,
			"clients_seen_5m", telemetryStats.ClientsSeen,
			"sessions_started_5m", telemetryStats.SessionsStarted,
			"connection_failures_5m", telemetryStats.Failures,
			"active_clients", telemetryStats.ActiveClients,
			"active_client_sessions", telemetryStats.ActiveSessions,
		)
	}
}
