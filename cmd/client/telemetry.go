package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
)

const (
	relayDialTimeout = 10 * time.Second
	probeWindow      = 5 * time.Second
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
	// brokerapi keeps each request above the two-second ECH phase so a network
	// that blackholes ECH still has time for verified plain-TLS fallback. The
	// outer context keeps the complete retry sweep at five seconds.
	api := brokerapi.NewClient(nil, brokerapi.Options{
		AppVersion: client.AppVersion(),
	})
	probeCtx, cancel := context.WithTimeout(ctx, probeWindow)
	defer cancel()
	for {
		started := time.Now()
		if err := api.ProbeHealth(probeCtx, brokerURL); err == nil {
			return time.Since(started).Milliseconds(), true
		}
		if probeCtx.Err() != nil {
			return 0, false
		}
		retryDelay := time.NewTimer(250 * time.Millisecond)
		select {
		case <-probeCtx.Done():
			retryDelay.Stop()
			return 0, false
		case <-retryDelay.C:
		}
	}
}

func healthURL(brokerURL string) (string, error) {
	return brokerapi.HealthURL(brokerURL)
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
