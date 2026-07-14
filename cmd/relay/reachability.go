package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"openrung/internal/relayruntime"
)

// resolveAutoMode runs the startup reachability probe and mutates cfg to either
// direct (with the observed public host) or tunnel. A probe that cannot run at
// all (hub HTTP API down) is inconclusive and defaults to tunnel, since tunnel
// mode's reconnect loop tolerates a temporarily-unavailable hub.
func resolveAutoMode(ctx context.Context, cfg *cliConfig) {
	hubHTTP := relayruntime.DeriveHubHTTPBase(cfg.HubHTTP, cfg.HubAddr, cfg.HubTLS)
	port := cfg.ListenPort
	slog.Info("auto-detecting reachability", "hub_http", hubHTTP, "probe_port", port)

	reachable, observed, err := relayruntime.DetectDirectReachable(ctx, hubHTTP, cfg.RegistrationToken, cfg.ListenHost, port, probeHTTPClient(cfg))
	if err != nil {
		slog.Warn("reachability probe unavailable; defaulting to tunnel mode", "error", err)
		cfg.TunnelMode = true
		return
	}
	if reachable {
		cfg.TunnelMode = false
		cfg.PublicHost = observed
		cfg.PublicPort = port
		slog.Info("auto-detect: directly reachable, using direct mode",
			"public", net.JoinHostPort(observed, strconv.Itoa(port)))
		return
	}
	cfg.TunnelMode = true
	slog.Info("auto-detect: not directly reachable, using tunnel mode")
}

// probeHTTPClient returns the HTTP client for the probe call. It honours a
// pre-set client (tests) and otherwise builds one that skips TLS verification
// only when -hub-insecure is set (matching the control channel).
func probeHTTPClient(cfg *cliConfig) *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if cfg.HubInsecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // gated behind -hub-insecure for self-signed hub certs (testing)
		}
	}
	return client
}
