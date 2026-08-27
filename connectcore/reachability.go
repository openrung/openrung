package connectcore

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// RelayTCPReachable measures the time to open a TCP connection to a relay's
// public endpoint, the Go analog of the mobile RelayReachability.checkTcp (it
// feeds relay_tcp_ms). The connection is closed immediately: only reachability
// and latency matter, the tunnel itself is sing-box's job.
//
// A zero timeout uses RelayTCPTimeout, the mobile-matched default; a caller
// with a different budget passes its own (the CLI keeps its historical 10s).
func RelayTCPReachable(ctx context.Context, host string, port int, timeout time.Duration) (int64, error) {
	return relayTCPReachable(ctx, host, port, timeout, func(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
		return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	})
}

// relayTCPReachable keeps the dial operation injectable for a deterministic
// timeout-contract test. Production callers always use RelayTCPReachable.
func relayTCPReachable(ctx context.Context, host string, port int, timeout time.Duration, dial func(context.Context, string, time.Duration) (net.Conn, error)) (int64, error) {
	if timeout <= 0 {
		timeout = RelayTCPTimeout
	}
	cleanHost := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	started := time.Now()
	conn, err := dial(ctx, net.JoinHostPort(cleanHost, strconv.Itoa(port)), timeout)
	if err != nil {
		// Wrap without masking the root cause so ClassifyError still labels the
		// telemetry (timeout, connection_refused, ...), like the mobile wrapper.
		return 0, fmt.Errorf("relay %s:%d is not reachable: %w", cleanHost, port, err)
	}
	_ = conn.Close()
	return time.Since(started).Milliseconds(), nil
}
