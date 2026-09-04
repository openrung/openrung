package connectcore

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRelayTCPReachableMeasuresLatency(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	// Brackets are stripped like the mobile check, so a bracketed literal from
	// the relay descriptor still dials.
	ms, err := RelayTCPReachable(context.Background(), "[127.0.0.1]", port, RelayTCPTimeout)
	if err != nil {
		t.Fatalf("reachable relay reported error: %v", err)
	}
	if ms < 0 {
		t.Fatalf("latency = %d", ms)
	}
}

func TestRelayTCPReachableWrapsRootCause(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close() // free the port so the dial is refused

	_, err = RelayTCPReachable(context.Background(), "127.0.0.1", port, RelayTCPTimeout)
	if err == nil {
		t.Fatal("expected a dial error")
	}
	if !strings.Contains(err.Error(), "is not reachable") {
		t.Fatalf("error missing wrapper context: %v", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("root cause lost for classification: %v", err)
	}
}

func TestRelayTCPReachableHonorsCallerTimeout(t *testing.T) {
	// Test the duration supplied to the dialer rather than relying on a public
	// TEST-NET address being black-holed: VPNs and corporate routes can make
	// that supposedly unreachable address connect successfully.
	const wantTimeout = 200 * time.Millisecond
	var gotTimeout time.Duration
	_, err := relayTCPReachable(context.Background(), "192.0.2.1", 443, wantTimeout,
		func(_ context.Context, _ string, timeout time.Duration) (net.Conn, error) {
			gotTimeout = timeout
			return nil, context.DeadlineExceeded
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapped deadline exceeded", err)
	}
	if gotTimeout != wantTimeout {
		t.Fatalf("dial timeout = %v, want caller timeout %v", gotTimeout, wantTimeout)
	}
}
