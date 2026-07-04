package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
)

const (
	relayDialTimeout = 10 * time.Second
	probeWindow      = 5 * time.Second
	probeTimeout     = 2 * time.Second
)

// newConnectManager builds the telemetry manager for a connect session.
// Telemetry is always on (parity with the mobile apps); if it cannot initialize
// it is best-effort disabled (nil) so connecting never fails on telemetry.
func newConnectManager(brokerURL string) *clienttelemetry.Manager {
	mgr, err := clienttelemetry.New(brokerURL, client.AppVersion(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry unavailable: %v\n", err)
		return nil
	}
	return mgr
}

// tcpReachMs measures the time to open a TCP connection to the relay, the CLI
// analog of Android's RelayReachability.checkTcp (relay_tcp_ms).
func tcpReachMs(ctx context.Context, host string, port int) (int64, error) {
	dialer := net.Dialer{Timeout: relayDialTimeout}
	started := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return time.Since(started).Milliseconds(), nil
}

// probeInternet issues a best-effort HTTP probe to the broker health endpoint to
// confirm connectivity after the tunnel starts. It retries within probeWindow.
// Returns the successful probe duration in ms, or ok=false if it never succeeds.
func probeInternet(ctx context.Context, brokerURL string) (int64, bool) {
	target, err := healthURL(brokerURL)
	if err != nil {
		return 0, false
	}
	httpClient := &http.Client{Timeout: probeTimeout}
	deadline := time.Now().Add(probeWindow)
	for {
		started := time.Now()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if reqErr != nil {
			return 0, false
		}
		resp, doErr := httpClient.Do(req)
		if doErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return time.Since(started).Milliseconds(), true
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func healthURL(brokerURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(brokerURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("broker URL must include scheme and host")
	}
	basePath := strings.Trim(parsed.Path, "/")
	parts := []string{"healthz"}
	if basePath != "" {
		parts = append([]string{basePath}, parts...)
	}
	parsed.Path = "/" + strings.Join(parts, "/")
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// errorType returns a short type name for an error, mirroring Android's use of
// the exception class simple name in error_type attributes.
func errorType(err error) string {
	if err == nil {
		return ""
	}
	name := fmt.Sprintf("%T", err)
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.TrimPrefix(name, "*")
}
