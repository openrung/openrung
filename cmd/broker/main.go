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
	"strconv"
	"strings"
	"syscall"
	"time"

	"openrung/internal/broker"
	"openrung/internal/buildinfo"
	"openrung/internal/wssbridge"
)

//go:embed VERSION
var baseVersion string

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
	geoIPEndpoint := flag.String("geoip-endpoint", envDefault("OPENRUNG_GEOIP_ENDPOINT", broker.DefaultGeoIPEndpoint), "IP geolocation HTTP endpoint for relay city/country lookups (relay host is appended); 'off' disables")
	flag.Parse()
	if *showVersion {
		fmt.Println(versionInfo())
		return nil
	}
	wssTicketSeed, err := parseOptionalSeed("OPENRUNG_WSS_TICKET_SIGNING_SEED", os.Getenv("OPENRUNG_WSS_TICKET_SIGNING_SEED"), ed25519.SeedSize)
	if err != nil {
		return err
	}
	// The user token key enables client enrollment/authentication; unset
	// leaves those routes unregistered.
	userTokenKey, err := parseOptionalSeed("OPENRUNG_USER_TOKEN_KEY", os.Getenv("OPENRUNG_USER_TOKEN_KEY"), 32)
	if err != nil {
		return err
	}
	accessTokenTTL, err := parseOptionalDuration("OPENRUNG_ACCESS_TOKEN_TTL")
	if err != nil {
		return err
	}
	enrollmentBurstLimit, err := parseOptionalInt("OPENRUNG_ENROLLMENT_BURST_LIMIT")
	if err != nil {
		return err
	}
	enrollmentDailyLimit, err := parseOptionalInt("OPENRUNG_ENROLLMENT_DAILY_LIMIT")
	if err != nil {
		return err
	}
	// Per-client relay credentials (access leases): lifetime and live caps.
	accessLeaseTTL, err := parseOptionalDuration("OPENRUNG_ACCESS_LEASE_TTL")
	if err != nil {
		return err
	}
	maxLeasesPerUser, err := parseOptionalInt("OPENRUNG_MAX_LEASES_PER_USER")
	if err != nil {
		return err
	}
	maxLeasesPerRelay, err := parseOptionalInt("OPENRUNG_MAX_LEASES_PER_RELAY")
	if err != nil {
		return err
	}
	// Datacenter source classification of new user identities: off unless
	// the operator points it at the refreshed data files.
	sourceClassifier, err := newSourceClassifier()
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

	// The operational API token gates the machine-to-machine relay inventory
	// endpoint. Unset leaves that route unregistered; a value shared with any
	// other broker credential is refused outright (see validateAPIToken).
	dashboardToken := os.Getenv("OPENRUNG_DASHBOARD_TOKEN")
	apiToken := os.Getenv("OPENRUNG_API_TOKEN")

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
	// Checked after the seeds are parsed so a token that reuses signing key
	// material is caught with the same message as a reused bearer token.
	if err := validateAPIToken(apiToken, []namedCredential{
		{name: "OPENRUNG_VOLUNTEER_TOKEN", value: registrationToken},
		{name: "OPENRUNG_FOUNDATION_TOKEN", value: foundationToken},
		{name: "OPENRUNG_DASHBOARD_TOKEN", value: dashboardToken},
		canonicalSeedCredential("OPENRUNG_RELAY_SIGNING_KEY", signingSeed),
		canonicalSeedCredential("OPENRUNG_WSS_TICKET_SIGNING_SEED", wssTicketSeed),
		canonicalSeedCredential("OPENRUNG_USER_TOKEN_KEY", userTokenKey),
	}); err != nil {
		return err
	}
	geoResolver := newGeoIPResolver(*geoIPEndpoint)
	// Inbound reachability probe of direct relays' advertised endpoints: on
	// by default; empty durations apply broker.Config's built-in defaults.
	relayProbeTimeout, err := parseOptionalDuration("OPENRUNG_RELAY_PROBE_TIMEOUT")
	if err != nil {
		return err
	}
	relayProbeInterval, err := parseOptionalDuration("OPENRUNG_RELAY_PROBE_INTERVAL")
	if err != nil {
		return err
	}
	relayProber, err := newRelayProber(os.Getenv("OPENRUNG_RELAY_PROBE"), relayProbeTimeout, relayProbeInterval)
	if err != nil {
		return err
	}
	store, err := newRelayStore(*relayStore, *relayDatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	telemetrySink, err := newTelemetrySink(*telemetryStore, *telemetryFile, *telemetryDatabaseURL)
	if err != nil {
		slog.Error("could not initialize telemetry storage", "error", err)
		return err
	}
	// Long-window cap on new relay identities per source IP; the empty default
	// applies broker.Config's built-in limit. Exempt the relay hub's addresses
	// so a wave of first-time tunnel volunteers is never throttled.
	maxNewRelayIDs, err := parseNewRelayIDCap(os.Getenv("OPENRUNG_MAX_NEW_RELAY_IDS_PER_IP_PER_DAY"))
	if err != nil {
		return err
	}
	rateLimits, err := parseRateLimits()
	if err != nil {
		return err
	}
	relayListDefaultLimit, relayListMaxLimit, err := parseRelayListLimits()
	if err != nil {
		return err
	}
	wssTicketTTL, wssTicketMaxStreams, err := parseWSSTicketBounds()
	if err != nil {
		return err
	}

	cfg := broker.Config{
		RegistrationToken: registrationToken,
		FoundationToken:   foundationToken,
		RelayLeaseTTL:     *leaseTTL,
		TelemetrySink:     telemetrySink,
		DashboardToken:    dashboardToken,
		Version:           buildinfo.Version(baseVersion),
		Revision:          buildinfo.Revision(),
		APIToken:          apiToken,
		// The only proxies trusted for forwarded client IPs; empty trusts none (prod: loopback Caddy).
		TrustedProxyCIDRs:          splitAndTrim(os.Getenv("OPENRUNG_TRUSTED_PROXY_CIDRS")),
		MaxNewRelayIDsPerIPPerDay:  maxNewRelayIDs,
		RegistrationCapExemptCIDRs: splitAndTrim(os.Getenv("OPENRUNG_REGISTRATION_CAP_EXEMPT_CIDRS")),
		GeoIP:                      geoResolver,
		RelayProber:                relayProber,
		RelayProbeInterval:         relayProbeInterval,
		SigningSeed:                signingSeed,
		WSSTicketSigningSeed:       wssTicketSeed,
		WSSTicketTTL:               wssTicketTTL,
		WSSTicketMaxStreams:        wssTicketMaxStreams,
		RateLimits:                 rateLimits,
		RelayListDefaultLimit:      relayListDefaultLimit,
		RelayListMaxLimit:          relayListMaxLimit,
		UserTokenKey:               userTokenKey,
		AccessTokenTTL:             accessTokenTTL,
		EnrollmentBudget:           broker.EnrollmentBudget{BurstLimit: enrollmentBurstLimit, DailyLimit: enrollmentDailyLimit},
		AccessLeaseTTL:             accessLeaseTTL,
		MaxLeasesPerUser:           maxLeasesPerUser,
		MaxLeasesPerRelay:          maxLeasesPerRelay,
		SourceClassifier:           sourceClassifier,
	}
	if len(userTokenKey) > 0 {
		slog.Info("client user authentication enabled")
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
	if sourceClassifier != nil {
		go sourceClassifier.Run(shutdownContext, 0)
	}
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

	slog.Info("starting broker", "version", buildinfo.Version(baseVersion), "revision", buildinfo.Revision(), "addr", *addr, "lease_ttl", leaseTTL.String(), "telemetry_store", *telemetryStore, "telemetry_file", *telemetryFile, "relay_store", *relayStore, "dashboard_enabled", dashboardToken != "", "operational_api_enabled", apiToken != "", "foundation_registration_enabled", foundationToken != "", "wss_ticket_issuance_enabled", len(wssTicketSeed) != 0, "status_interval", statusInterval.String(), "geoip_enabled", geoResolver != nil, "relay_probe_enabled", relayProber != nil, "source_classification_enabled", sourceClassifier != nil)
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

func versionInfo() string {
	return buildinfo.Info("broker", baseVersion)
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

func newRelayStore(storeMode, databaseURL string) (broker.RelayStore, error) {
	switch strings.ToLower(strings.TrimSpace(storeMode)) {
	case "", "memory":
		return broker.NewStore(), nil
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return broker.NewPostgresStore(ctx, databaseURL)
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

// newRelayProber builds the inbound reachability prober from
// OPENRUNG_RELAY_PROBE, or nil (probing disabled, every relay listed on its
// lease alone) for an "off"-style mode. Zero durations mean the built-in
// defaults; anything else malformed refuses to start rather than silently
// running without the check or with a nonsensical bound.
func newRelayProber(mode string, timeout, interval time.Duration) (broker.RelayProber, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "disabled", "none", "false", "0":
		return nil, nil
	case "", "on", "enabled", "true", "1":
	default:
		return nil, errors.New("OPENRUNG_RELAY_PROBE must be on or off")
	}
	if timeout < 0 {
		return nil, errors.New("OPENRUNG_RELAY_PROBE_TIMEOUT must be a positive duration like 5s")
	}
	if interval < 0 {
		return nil, errors.New("OPENRUNG_RELAY_PROBE_INTERVAL must be a positive duration like 5m")
	}
	return broker.NewTCPRelayProber(timeout), nil
}

// newSourceClassifier builds the datacenter source classifier from
// OPENRUNG_SOURCE_ASN_DB and OPENRUNG_DATACENTER_ASN_LIST, or returns nil
// (classification disabled) when neither is set. Setting only one, a
// malformed exempt CIDR or max age refuses to start; data that cannot be
// loaded yet does not (enrollment answers 503 until it can).
func newSourceClassifier() (*broker.SourceClassifier, error) {
	table := strings.TrimSpace(os.Getenv("OPENRUNG_SOURCE_ASN_DB"))
	list := strings.TrimSpace(os.Getenv("OPENRUNG_DATACENTER_ASN_LIST"))
	if table == "" && list == "" {
		if strings.TrimSpace(os.Getenv("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS")) != "" {
			slog.Warn("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS is set but datacenter source classification is disabled (OPENRUNG_SOURCE_ASN_DB unset)")
		}
		return nil, nil
	}
	maxAge, err := parseOptionalDuration("OPENRUNG_SOURCE_ASN_MAX_AGE")
	if err != nil {
		return nil, err
	}
	return broker.NewSourceClassifier(broker.SourceClassifierConfig{
		ASNDBPath:             table,
		DatacenterASNListPath: list,
		ExemptCIDRs:           splitAndTrim(os.Getenv("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS")),
		MaxAge:                maxAge,
	})
}

// parseRateLimits reads the OPENRUNG_RATE_LIMIT_* per-client budgets
// ("<per-second rate>:<burst>"); unset ones keep the broker's defaults.
func parseRateLimits() (broker.RateLimits, error) {
	var limits broker.RateLimits
	for _, entry := range []struct {
		name   string
		target *broker.RateLimit
	}{
		{"OPENRUNG_RATE_LIMIT_RELAY_LIST", &limits.RelayList},
		{"OPENRUNG_RATE_LIMIT_WSS_TICKET", &limits.WSSTicket},
		{"OPENRUNG_RATE_LIMIT_TELEMETRY", &limits.Telemetry},
		{"OPENRUNG_RATE_LIMIT_USER_AUTH", &limits.UserAuth},
		{"OPENRUNG_RATE_LIMIT_ACCESS", &limits.Access},
		{"OPENRUNG_RATE_LIMIT_RELAY_REGISTRATION", &limits.RelayRegistration},
		{"OPENRUNG_RATE_LIMIT_SPEED_TEST", &limits.SpeedTest},
	} {
		limit, err := broker.ParseRateLimit(os.Getenv(entry.name))
		if err != nil {
			return broker.RateLimits{}, fmt.Errorf("%s: %w", entry.name, err)
		}
		*entry.target = limit
	}
	return limits, nil
}

// parseRelayListLimits reads the relay-list page sizes; zero keeps the
// broker's default for that bound.
func parseRelayListLimits() (defaultLimit, maxLimit int, err error) {
	if defaultLimit, err = parseOptionalInt("OPENRUNG_RELAY_LIST_DEFAULT_LIMIT"); err != nil {
		return 0, 0, err
	}
	if maxLimit, err = parseOptionalInt("OPENRUNG_RELAY_LIST_MAX_LIMIT"); err != nil {
		return 0, 0, err
	}
	if defaultLimit < 0 || maxLimit < 0 {
		return 0, 0, errors.New("OPENRUNG_RELAY_LIST_DEFAULT_LIMIT and OPENRUNG_RELAY_LIST_MAX_LIMIT must be positive")
	}
	if defaultLimit > 0 && maxLimit > 0 && defaultLimit > maxLimit {
		return 0, 0, errors.New("OPENRUNG_RELAY_LIST_DEFAULT_LIMIT must not exceed OPENRUNG_RELAY_LIST_MAX_LIMIT")
	}
	return defaultLimit, maxLimit, nil
}

// parseWSSTicketBounds reads the per-ticket lifetime and stream budget; zero
// keeps the broker's default. Out-of-range values fail startup here rather
// than in the ticket issuer.
func parseWSSTicketBounds() (time.Duration, int, error) {
	ttl, err := parseOptionalDuration("OPENRUNG_WSS_TICKET_TTL")
	if err != nil {
		return 0, 0, err
	}
	if ttl != 0 && (ttl < 2*time.Second || ttl > wssbridge.MaxTicketLifetime) {
		return 0, 0, fmt.Errorf("OPENRUNG_WSS_TICKET_TTL must be within [2s, %s]", wssbridge.MaxTicketLifetime)
	}
	streams, err := parseOptionalInt("OPENRUNG_WSS_TICKET_MAX_STREAMS")
	if err != nil {
		return 0, 0, err
	}
	if streams != 0 && (streams < 1 || streams > wssbridge.MaxTicketStreams) {
		return 0, 0, fmt.Errorf("OPENRUNG_WSS_TICKET_MAX_STREAMS must be within [1, %d]", wssbridge.MaxTicketStreams)
	}
	return ttl, streams, nil
}

// parseNewRelayIDCap maps OPENRUNG_MAX_NEW_RELAY_IDS_PER_IP_PER_DAY onto
// broker.Config's convention: empty keeps the built-in default, "off" (or any
// negative number) disables the cap, and a positive integer overrides it.
// Anything else refuses to start rather than silently running unguarded.
func parseNewRelayIDCap(value string) (int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, nil
	}
	if value == "off" {
		return -1, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed == 0 {
		return 0, errors.New("OPENRUNG_MAX_NEW_RELAY_IDS_PER_IP_PER_DAY must be a non-zero integer or 'off'")
	}
	return parsed, nil
}

// namedCredential pairs a broker credential with the environment variable it
// came from, so validateAPIToken can name the collision it refuses instead of
// making the operator guess which value they reused.
type namedCredential struct {
	name  string
	value string
}

// canonicalSeedCredential compares the API token with the actual parsed seed,
// not its raw environment spelling. The base64 decoder ignores CR/LF and the
// optional WSS parser also trims surrounding whitespace, so comparing the raw
// strings would let two spellings of the same secret evade the collision guard.
func canonicalSeedCredential(name string, seed []byte) namedCredential {
	if len(seed) == 0 {
		return namedCredential{name: name}
	}
	return namedCredential{name: name, value: base64.StdEncoding.EncodeToString(seed)}
}

// validateAPIToken refuses an operational API token that duplicates another
// broker credential. Each collision would silently widen that credential's
// blast radius in a different direction: sharing a registration token would
// hand every relay operator the fleet inventory, sharing the dashboard token
// would put a browser-session secret into scripted machine-to-machine callers
// (and vice versa), and sharing either signing seed would leak private key
// material to whoever merely needed to read the inventory — the one that could
// forge a signed directory. Empty is not a collision: an unset token disables
// the endpoint, and an unset peer credential is simply not configured.
func validateAPIToken(apiToken string, others []namedCredential) error {
	if apiToken == "" {
		return nil
	}
	for _, other := range others {
		if other.value == "" {
			continue
		}
		if apiToken == other.value {
			return fmt.Errorf("OPENRUNG_API_TOKEN must differ from %s: a shared value would extend that credential's reach to every holder of the operational API token", other.name)
		}
	}
	return nil
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

// parseOptionalSeed decodes an optional standard-base64 secret of exactly
// size bytes; empty means unset.
func parseOptionalSeed(name, value string, size int) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	seed, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be standard base64", name)
	}
	if len(seed) != size {
		return nil, fmt.Errorf("%s must decode to %d bytes", name, size)
	}
	return append([]byte(nil), seed...), nil
}

// parseOptionalDuration reads an optional Go duration from the environment;
// empty means zero (the component's default).
func parseOptionalDuration(name string) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 24h: %w", name, err)
	}
	return parsed, nil
}

// parseOptionalInt reads an optional integer from the environment; empty
// means zero (the component's default). Negative values are passed through,
// since some limits use them to mean "disabled".
func parseOptionalInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
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
