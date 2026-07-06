package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestMaxConcurrentConns(t *testing.T) {
	cases := []struct {
		maxSessions int
		want        int
	}{
		{1, perTunnelConnFloor},   // 256 < floor → floor
		{4, 4 * connsPerSession},  // within range
		{1000, perTunnelConnCeil}, // far above ceil → ceil
	}
	for _, c := range cases {
		tn := &tunnel{registerReq: relay.RegisterRequest{MaxSessions: c.maxSessions}}
		if got := tn.maxConcurrentConns(); got != c.want {
			t.Errorf("maxConcurrentConns(MaxSessions=%d) = %d, want %d", c.maxSessions, got, c.want)
		}
	}
}

// TestHubReapsSilentClientConnection is the regression test for the unbounded
// client-connection DoS: a connection that opens the public port and never sends
// the handshake must be reaped, not held forever pinning a goroutine + stream.
func TestHubReapsSilentClientConnection(t *testing.T) {
	orig := clientHandshakeTimeout
	clientHandshakeTimeout = 200 * time.Millisecond
	defer func() { clientHandshakeTimeout = orig }()

	echoHost, echoPort := startEchoServer(t)
	registrar := &fakeRegistrar{relayID: "relay_reap"}
	_, controlAddr, _ := startTestHub(t, registrar, "secret")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ackCh := make(chan HelloAckFrame, 1)
	client := &Client{
		HubAddr: controlAddr,
		Hello: HelloFrame{
			Token: "secret", RealityPublicKey: "pk", ShortID: "sid", ServerName: "www.example.com",
			ClientID: "cid", Flow: relay.FlowVision, ExitMode: relay.ExitModeDirect,
			MaxSessions: 4, MaxMbps: 10, VolunteerVersion: "test",
		},
		TargetHost:   echoHost,
		TargetPort:   echoPort,
		ReconnectMin: 10 * time.Millisecond,
		Logger:       discardLogger(),
		OnRegistered: func(a HelloAckFrame) {
			select {
			case ackCh <- a:
			default:
			}
		},
	}
	go func() { _ = client.Run(ctx) }()

	var ack HelloAckFrame
	select {
	case ack = <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tunnel")
	}

	// Open the public port and stay silent. The hub must close us after the
	// handshake window; a clean EOF (not our own read-deadline firing)
	// distinguishes "hub reaped us" from "reaping is broken".
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ack.PublicPort)), time.Second)
	if err != nil {
		t.Fatalf("dial public port: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF from hub reaping the silent connection, got %v after %s", err, time.Since(start))
	}

	// A connection that DOES send the handshake is still spliced and echoes fine
	// even with the short handshake window.
	if err := dialEchoThroughTunnel(ack.PublicPort); err != nil {
		t.Fatalf("active connection should still echo: %v", err)
	}
}
