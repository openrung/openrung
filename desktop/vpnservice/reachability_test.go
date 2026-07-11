package vpnservice

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
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
	ms, err := relayTCPReachable(context.Background(), "[127.0.0.1]", port)
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

	_, err = relayTCPReachable(context.Background(), "127.0.0.1", port)
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
