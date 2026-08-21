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
	// A caller's own budget (the CLI passes 10s) bounds the dial instead of
	// RelayTCPTimeout: a short one against a black-holed address gives up long
	// before the 5s default would.
	started := time.Now()
	if _, err := RelayTCPReachable(context.Background(), "192.0.2.1", 443, 200*time.Millisecond); err == nil {
		t.Fatal("expected a dial error for a black-holed address")
	}
	if elapsed := time.Since(started); elapsed >= RelayTCPTimeout {
		t.Fatalf("dial took %v, want the caller's shorter budget", elapsed)
	}
}
