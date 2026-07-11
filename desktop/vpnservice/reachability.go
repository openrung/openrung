package vpnservice

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"openrung/desktop/config"
)

// relayTCPReachable measures the time to open a TCP connection to the relay's
// public endpoint, the desktop analog of the mobile RelayReachability.checkTcp
// (it feeds relay_tcp_ms). The connection is closed immediately: only
// reachability and latency matter, the tunnel itself is sing-box's job.
func relayTCPReachable(ctx context.Context, host string, port int) (int64, error) {
	cleanHost := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	dialer := net.Dialer{Timeout: config.RelayTCPTimeout}
	started := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cleanHost, strconv.Itoa(port)))
	if err != nil {
		// Wrap without masking the root cause so ClassifyError still labels the
		// telemetry (timeout, connection_refused, ...), like the mobile wrapper.
		return 0, fmt.Errorf("relay %s:%d is not reachable: %w", cleanHost, port, err)
	}
	_ = conn.Close()
	return time.Since(started).Milliseconds(), nil
}
