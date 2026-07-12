package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeRegistrar struct {
	mu            sync.Mutex
	registerCount int
	heartbeats    int
	lastReq       relay.RegisterRequest
	relayID       string
	// relayIDs, when non-empty, are consumed one per Register call before
	// falling back to relayID.
	relayIDs []string
	// failHeartbeatOnce is returned by the next Heartbeat call, then cleared.
	failHeartbeatOnce error
	// releaseHeartbeat, when non-nil, holds the Heartbeat call that returns
	// failHeartbeatOnce until the channel is closed. This lets a test observe the
	// pre-re-registration registry state deterministically instead of racing the
	// re-registration that the not-found error triggers.
	releaseHeartbeat chan struct{}
}

func (f *fakeRegistrar) Register(_ context.Context, req relay.RegisterRequest) (RelayRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCount++
	f.lastReq = req
	id := f.relayID
	if len(f.relayIDs) > 0 {
		id = f.relayIDs[0]
		f.relayIDs = f.relayIDs[1:]
	}
	if id == "" {
		id = "relay_test"
	}
	return RelayRegistration{RelayID: id, PublicHost: req.PublicHost, PublicPort: req.PublicPort, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeRegistrar) Heartbeat(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.heartbeats++
	err := f.failHeartbeatOnce
	if err == nil {
		f.mu.Unlock()
		return nil
	}
	f.failHeartbeatOnce = nil
	gate := f.releaseHeartbeat
	f.mu.Unlock()
	// Hold the not-found response until the test releases the gate, so the
	// assertion on the initial relay entry cannot race the re-registration this
	// error triggers. Honor ctx so a failed test tears the goroutine down.
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeRegistrar) stats() (registers, heartbeats int, lastReq relay.RegisterRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCount, f.heartbeats, f.lastReq
}

func startEchoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// startTestHub spins up a hub on a plaintext loopback control listener with a
// single-port allocator and returns the control address and allocator.
func startTestHub(t *testing.T, registrar Registrar, token string) (*Hub, string, *PortAllocator) {
	t.Helper()
	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	port := freePort(t)
	alloc, err := NewPortAllocator(port, port)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}
	hub := &Hub{
		ControlListener:   controlLn,
		PublicHost:        "127.0.0.1",
		PublicBindHost:    "127.0.0.1",
		Allocator:         alloc,
		Registrar:         registrar,
		Token:             token,
		HeartbeatInterval: 25 * time.Millisecond,
		HandshakeTimeout:  2 * time.Second,
		Logger:            discardLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Serve(ctx) }()
	t.Cleanup(cancel)
	return hub, controlLn.Addr().String(), alloc
}

func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func dialEchoThroughTunnel(port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var lastErr error
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		msg := []byte("hello-through-tunnel")
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		if string(buf) != string(msg) {
			return fmt.Errorf("echo mismatch: got %q", buf)
		}
		return nil
	}
	return lastErr
}

func TestHubClientEndToEnd(t *testing.T) {
	echoHost, echoPort := startEchoServer(t)
	registrar := &fakeRegistrar{relayID: "relay_abc"}
	_, controlAddr, alloc := startTestHub(t, registrar, "secret")

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()

	ackCh := make(chan HelloAckFrame, 1)
	client := &Client{
		HubAddr: controlAddr,
		Hello: HelloFrame{
			Token:            "secret",
			RealityPublicKey: "pk",
			ShortID:          "sid",
			ServerName:       "www.example.com",
			ClientID:         "cid",
			Flow:             relay.FlowVision,
			ExitMode:         relay.ExitModeDirect,
			MaxSessions:      4,
			MaxMbps:          10,
			Label:            "lbl",
			VolunteerVersion: "test",
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
	go func() { _ = client.Run(clientCtx) }()

	var ack HelloAckFrame
	select {
	case ack = <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tunnel establishment")
	}
	if !ack.OK || ack.PublicPort == 0 {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	if ack.RelayID != "relay_abc" {
		t.Fatalf("relay_id = %q, want relay_abc", ack.RelayID)
	}

	if err := dialEchoThroughTunnel(ack.PublicPort); err != nil {
		t.Fatalf("echo through tunnel: %v", err)
	}

	registers, _, lastReq := registrar.stats()
	if registers != 1 {
		t.Fatalf("register count = %d, want 1", registers)
	}
	if lastReq.RealityPublicKey != "pk" || lastReq.ShortID != "sid" || lastReq.ClientID != "cid" {
		t.Fatalf("register metadata not forwarded: %+v", lastReq)
	}
	if lastReq.Transport != relay.TransportTunnel {
		t.Fatalf("transport = %q, want %q", lastReq.Transport, relay.TransportTunnel)
	}
	if lastReq.PublicPort != ack.PublicPort || lastReq.PublicHost != "127.0.0.1" {
		t.Fatalf("register endpoint mismatch: host=%q port=%d", lastReq.PublicHost, lastReq.PublicPort)
	}
	if lastReq.ExitHost != "127.0.0.1" {
		t.Fatalf("exit_host = %q, want the volunteer's source IP 127.0.0.1", lastReq.ExitHost)
	}

	if !eventually(2*time.Second, func() bool {
		_, hb, _ := registrar.stats()
		return hb >= 1
	}) {
		t.Fatal("expected at least one heartbeat while connected")
	}

	// Teardown: cancel the client; the hub should free the allocated port.
	clientCancel()
	if !eventually(3*time.Second, func() bool { return alloc.InUse() == 0 }) {
		t.Fatalf("port not released after teardown, InUse=%d", alloc.InUse())
	}
}

func TestHubReregistersWhenBrokerForgetsRelay(t *testing.T) {
	echoHost, echoPort := startEchoServer(t)
	registrar := &fakeRegistrar{
		relayIDs:          []string{"relay_old", "relay_new"},
		failHeartbeatOnce: fmt.Errorf("broker /api/v1/volunteers/relay_old/heartbeat: %w", ErrRelayNotFound),
		releaseHeartbeat:  make(chan struct{}),
	}
	hub, controlAddr, _ := startTestHub(t, registrar, "secret")

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()

	ackCh := make(chan HelloAckFrame, 1)
	client := &Client{
		HubAddr: controlAddr,
		Hello: HelloFrame{
			Token:            "secret",
			RealityPublicKey: "pk",
			ShortID:          "sid",
			ServerName:       "www.example.com",
			ClientID:         "cid",
			Flow:             relay.FlowVision,
			ExitMode:         relay.ExitModeDirect,
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
	go func() { _ = client.Run(clientCtx) }()

	var ack HelloAckFrame
	select {
	case ack = <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tunnel establishment")
	}
	if ack.RelayID != "relay_old" {
		t.Fatalf("relay_id = %q, want relay_old", ack.RelayID)
	}
	// The hub publishes the tunnel to its registry after sending the ack, so wait
	// for the initial relay_old entry rather than reading it instantaneously. The
	// gated heartbeat below guarantees re-registration has not yet removed it.
	if !eventually(2*time.Second, func() bool { return hub.lookupTunnel("relay_old") != nil }) {
		t.Fatal("registry missing initial relay_old entry")
	}

	// Now let the first heartbeat return not-found; the hub must re-register.
	close(registrar.releaseHeartbeat)
	if !eventually(3*time.Second, func() bool {
		registers, _, _ := registrar.stats()
		return registers == 2
	}) {
		registers, hb, _ := registrar.stats()
		t.Fatalf("no re-registration after not-found heartbeat (registers=%d heartbeats=%d)", registers, hb)
	}

	// Re-registration reuses the original request, including the exit host.
	_, _, lastReq := registrar.stats()
	if lastReq.ExitHost != "127.0.0.1" || lastReq.PublicPort != ack.PublicPort {
		t.Fatalf("re-register request mismatch: exit_host=%q port=%d", lastReq.ExitHost, lastReq.PublicPort)
	}

	// The registry is re-keyed to the new relay ID.
	if !eventually(2*time.Second, func() bool { return hub.lookupTunnel("relay_new") != nil }) {
		t.Fatal("registry not re-keyed to relay_new")
	}
	if hub.lookupTunnel("relay_old") != nil {
		t.Fatal("stale relay_old entry left in registry")
	}

	// The public listener and yamux session were untouched: traffic still flows,
	// and subsequent heartbeats (now against the new ID) succeed.
	if err := dialEchoThroughTunnel(ack.PublicPort); err != nil {
		t.Fatalf("echo through tunnel after re-registration: %v", err)
	}
	_, hbBefore, _ := registrar.stats()
	if !eventually(2*time.Second, func() bool {
		_, hb, _ := registrar.stats()
		return hb > hbBefore
	}) {
		t.Fatal("heartbeats stopped after re-registration")
	}
	registers, _, _ := registrar.stats()
	if registers != 2 {
		t.Fatalf("register count = %d, want exactly 2", registers)
	}
}

func TestHubRejectsBadToken(t *testing.T) {
	registrar := &fakeRegistrar{}
	_, controlAddr, alloc := startTestHub(t, registrar, "secret")

	conn, err := net.Dial("tcp", controlAddr)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	if err := writeFrame(conn, HelloFrame{ProtocolVersion: ProtocolVersion, Token: "wrong"}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ack HelloAckFrame
	if err := readFrame(conn, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.OK {
		t.Fatal("expected rejection for bad token")
	}
	if registers, _, _ := registrar.stats(); registers != 0 {
		t.Fatalf("register called on auth failure: %d", registers)
	}
	if alloc.InUse() != 0 {
		t.Fatalf("port allocated on auth failure: %d", alloc.InUse())
	}
}

func TestHubRejectsProtocolMismatch(t *testing.T) {
	registrar := &fakeRegistrar{}
	_, controlAddr, _ := startTestHub(t, registrar, "")

	conn, err := net.Dial("tcp", controlAddr)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	if err := writeFrame(conn, HelloFrame{ProtocolVersion: ProtocolVersion + 1}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ack HelloAckFrame
	if err := readFrame(conn, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.OK {
		t.Fatal("expected rejection for protocol mismatch")
	}
}
