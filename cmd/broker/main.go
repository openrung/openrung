package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"openrung/internal/broker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("broker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	leaseTTL := flag.Duration("lease-ttl", 3*time.Minute, "volunteer relay lease TTL")
	telemetryFile := flag.String("telemetry-file", "openrung-telemetry.jsonl", "append-only client telemetry JSONL file")
	statusInterval := flag.Duration("status-interval", time.Minute, "interval for broker network status logs; 0 disables")
	relayStore := flag.String("relay-store", envDefault("OPENRUNG_RELAY_STORE", "memory"), "relay state backend: memory or postgres")
	relayDatabaseURL := flag.String("relay-database-url", os.Getenv("OPENRUNG_RELAY_DATABASE_URL"), "PostgreSQL database URL for relay state")
	relayRanking := flag.String("relay-ranking", envDefault("OPENRUNG_RELAY_RANKING", "global"), "relay ranking mode: global or legacy")
	geoIPEndpoint := flag.String("geoip-endpoint", envDefault("OPENRUNG_GEOIP_ENDPOINT", broker.DefaultGeoIPEndpoint), "IP geolocation HTTP endpoint for relay city/country lookups (relay host is appended); 'off' disables")
	flag.Parse()

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
	geoResolver := newGeoIPResolver(*geoIPEndpoint)
	store, err := newRelayStore(*relayStore, *relayDatabaseURL, rankingMode)
	if err != nil {
		return err
	}
	defer store.Close()

	telemetrySink, err := broker.NewJSONLTelemetrySink(*telemetryFile)
	if err != nil {
		slog.Error("could not initialize telemetry storage", "error", err)
		return err
	}
	handler := broker.NewServer(store, broker.Config{
		RegistrationToken: registrationToken,
		VolunteerLeaseTTL: *leaseTTL,
		TelemetrySink:     telemetrySink,
		TelemetryReader:   telemetrySink,
		DashboardToken:    os.Getenv("OPENRUNG_DASHBOARD_TOKEN"),
		// Cloudflare's published ranges are trusted by default; add more (e.g. an upstream LB) here.
		TrustedProxyCIDRs: splitAndTrim(os.Getenv("OPENRUNG_TRUSTED_PROXY_CIDRS")),
		GeoIP:             geoResolver,
	})
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

	slog.Info("starting broker", "addr", *addr, "lease_ttl", leaseTTL.String(), "telemetry_file", *telemetryFile, "relay_store", *relayStore, "relay_ranking", rankingMode, "dashboard_enabled", os.Getenv("OPENRUNG_DASHBOARD_TOKEN") != "", "status_interval", statusInterval.String(), "geoip_enabled", geoResolver != nil)
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
			slog.Info("volunteer expired", "relay_id", desc.ID, "last_heartbeat_at", desc.LastHeartbeatAt)
		}
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
			"active_volunteers", storeStats.ActiveVolunteers,
			"advertised_session_capacity", storeStats.AdvertisedSessionCapacity,
			"clients_seen_5m", telemetryStats.ClientsSeen,
			"sessions_started_5m", telemetryStats.SessionsStarted,
			"connection_failures_5m", telemetryStats.Failures,
			"active_clients", telemetryStats.ActiveClients,
			"active_client_sessions", telemetryStats.ActiveSessions,
		)
	}
}
