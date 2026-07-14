package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/openrung/openrung/punchcore"

	"openrung/internal/relayhub"
	"openrung/internal/tunnel"
)

func main() {
	if err := run(); err != nil {
		slog.Error("relay hub stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}

	// Fail closed: an empty token disables relay authentication, so any caller
	// could register a relay through this hub. Require a token unless the
	// operator explicitly opts into an open hub.
	if cfg.Token == "" && !envTrue("OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS") {
		return errors.New("OPENRUNG_VOLUNTEER_TOKEN is empty: refusing to start an open hub where any caller can register a relay. Set OPENRUNG_VOLUNTEER_TOKEN, or set OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true to run open intentionally")
	}

	alloc, err := tunnel.NewPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd)
	if err != nil {
		return err
	}

	listener, err := controlListener(cfg)
	if err != nil {
		return err
	}
	defer listener.Close()

	hub := &tunnel.Hub{
		ControlListener:   listener,
		PublicHost:        cfg.PublicHost,
		PublicBindHost:    cfg.PublicBindHost,
		Allocator:         alloc,
		Registrar:         tunnel.NewBrokerRegistrar(cfg.BrokerURL, cfg.Token, nil),
		Token:             cfg.Token,
		HeartbeatInterval: cfg.HeartbeatInterval,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.HTTPEnabled() {
		mux := http.NewServeMux()

		// The reachability prober is always available on the HTTP API so
		// relays can auto-detect direct vs tunnel even when punch is off.
		tunnel.NewReachabilityProber(cfg.Token, slog.Default()).Register(mux)

		if cfg.PunchEnabled() {
			reflector, err := punchcore.NewReflector(cfg.ReflectorAddrs, slog.Default())
			if err != nil {
				return fmt.Errorf("start punch reflector: %w", err)
			}
			defer reflector.Close()
			// Advertise the public reflector addresses when configured (host whose
			// public IP is NAT'd off the NIC), else the actual bound addresses.
			advertise := cfg.ReflectorAdvertise
			if len(advertise) == 0 {
				advertise = reflector.Addrs()
			}
			hub.ReflectorAddrs = advertise

			// Advertise the punch coordinator URL to clients (correct scheme +
			// public host + port) so they don't derive a wrong one.
			if _, httpPort, err := net.SplitHostPort(cfg.HTTPAddr); err == nil {
				scheme := "http"
				if cfg.TLSEnabled() {
					scheme = "https"
				}
				hub.PunchEndpoint = scheme + "://" + net.JoinHostPort(cfg.PublicHost, httpPort)
			}

			coordinator, err := tunnel.NewPunchCoordinator(hub, reflector, cfg.PunchTTL, slog.Default())
			if err != nil {
				return fmt.Errorf("start punch coordinator: %w", err)
			}
			coordinator.Register(mux)
			slog.Info("punch coordinator enabled", "reflectors", hub.ReflectorAddrs, "ttl", cfg.PunchTTL)
		}

		httpServer := &http.Server{
			Addr:         cfg.HTTPAddr,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 15 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
		httpTLS := cfg.TLSEnabled()
		if !httpTLS {
			slog.Warn("hub http api is running without TLS; the volunteer token will be sent in plaintext")
		}
		go func() {
			var err error
			if httpTLS {
				// Reuse the control-channel certificate for the HTTP API.
				err = httpServer.ListenAndServeTLS(cfg.TLSCertPath, cfg.TLSKeyPath)
			} else {
				err = httpServer.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("hub http server stopped", "error", err)
			}
		}()
		slog.Info("hub http api enabled", "http_addr", cfg.HTTPAddr, "tls", httpTLS, "punch", cfg.PunchEnabled())
	}

	slog.Info("starting relay hub",
		"control_addr", cfg.ControlAddr,
		"public_host", cfg.PublicHost,
		"port_range", fmt.Sprintf("%d-%d", cfg.PortRangeStart, cfg.PortRangeEnd),
		"broker", cfg.BrokerURL,
		"tls", cfg.TLSEnabled(),
		"auth_required", cfg.Token != "",
		"punch", cfg.PunchEnabled(),
	)
	if !cfg.TLSEnabled() {
		slog.Warn("relay hub control channel is running without TLS; volunteer tokens will be sent in plaintext")
	}

	return hub.Serve(ctx)
}

func parseConfig() (relayhub.Config, error) {
	controlAddr := flag.String("control-addr", envDefault("OPENRUNG_HUB_CONTROL_ADDR", ":9443"), "control listener address dialed by volunteer-run relays")
	publicHost := flag.String("public-host", os.Getenv("OPENRUNG_HUB_PUBLIC_HOST"), "public hostname or IP advertised to clients")
	publicBindHost := flag.String("public-bind-host", os.Getenv("OPENRUNG_HUB_PUBLIC_BIND_HOST"), "interface the per-tunnel public listeners bind to; empty means all interfaces")
	portRange := flag.String("port-range", envDefault("OPENRUNG_HUB_PORT_RANGE", "20000-20100"), "public TCP port range as start-end")
	brokerURL := flag.String("broker", envDefault("OPENRUNG_BROKER_URL", "http://localhost:8080"), "broker base URL")
	token := flag.String("token", os.Getenv("OPENRUNG_VOLUNTEER_TOKEN"), "shared volunteer-class relay/broker bearer token")
	tlsCert := flag.String("tls-cert", os.Getenv("OPENRUNG_HUB_TLS_CERT"), "TLS certificate file for the control channel")
	tlsKey := flag.String("tls-key", os.Getenv("OPENRUNG_HUB_TLS_KEY"), "TLS key file for the control channel")
	heartbeat := flag.Duration("heartbeat-interval", envDurationDefault("OPENRUNG_HUB_HEARTBEAT_INTERVAL", 30*time.Second), "broker heartbeat interval for live relays")
	httpAddr := flag.String("http-addr", envDefault("OPENRUNG_HUB_HTTP_ADDR", os.Getenv("OPENRUNG_HUB_PUNCH_ADDR")), "address for the hub HTTP API — reachability prober + (with reflectors) NAT-punch coordinator, e.g. :9444; empty disables it")
	reflectorAddrs := flag.String("reflector-addrs", os.Getenv("OPENRUNG_HUB_REFLECTOR_ADDRS"), "comma-separated UDP reflector BIND host:port addresses (must exist on the NIC; use the private IP or a wildcard on a NAT'd host)")
	reflectorAdvertise := flag.String("reflector-advertise", os.Getenv("OPENRUNG_HUB_REFLECTOR_ADVERTISE"), "comma-separated PUBLIC reflector host:port addresses announced to peers, matched to -reflector-addrs; empty advertises the bound addresses")
	punchTTL := flag.Duration("punch-ttl", envDurationDefault("OPENRUNG_HUB_PUNCH_TTL", 6*time.Second), "punch time budget handed to peers")
	flag.Parse()

	start, end, err := relayhub.ParsePortRange(*portRange)
	if err != nil {
		return relayhub.Config{}, err
	}

	cfg := relayhub.Config{
		ControlAddr:        *controlAddr,
		PublicHost:         *publicHost,
		PublicBindHost:     *publicBindHost,
		PortRangeStart:     start,
		PortRangeEnd:       end,
		BrokerURL:          *brokerURL,
		Token:              *token,
		TLSCertPath:        *tlsCert,
		TLSKeyPath:         *tlsKey,
		HeartbeatInterval:  *heartbeat,
		HTTPAddr:           *httpAddr,
		ReflectorAddrs:     relayhub.ParseReflectorAddrs(*reflectorAddrs),
		ReflectorAdvertise: relayhub.ParseReflectorAddrs(*reflectorAdvertise),
		PunchTTL:           *punchTTL,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return relayhub.Config{}, err
	}
	return cfg, nil
}

func controlListener(cfg relayhub.Config) (net.Listener, error) {
	listener, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on control address: %w", err)
	}
	if !cfg.TLSEnabled() {
		return listener, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}), nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDurationDefault(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
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
