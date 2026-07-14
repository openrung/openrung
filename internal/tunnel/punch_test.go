package tunnel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openrung/openrung/punchcore"

	"openrung/internal/punch"
	"openrung/internal/relay"
)

// TestTypedRelayStillEchoes verifies that when stream typing is negotiated the
// hub prefixes data streams with the discriminator and the relay strips it,
// so ordinary relayed traffic is unaffected even with typing on.
func TestTypedRelayStillEchoes(t *testing.T) {
	echoHost, echoPort := startEchoServer(t)
	registrar := &fakeRegistrar{relayID: "relay_typed"}
	_, controlAddr, _ := startTestHub(t, registrar, "secret") // hub has no reflector → punch unavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ackCh := make(chan HelloAckFrame, 1)
	client := &Client{
		HubAddr: controlAddr,
		Hello: HelloFrame{
			Token: "secret", RealityPublicKey: "pk", ShortID: "sid", ServerName: "www.example.com",
			ClientID: "cid", Flow: relay.FlowVision, ExitMode: relay.ExitModeDirect,
			MaxSessions: 4, MaxMbps: 10, RelayVersion: "test",
			StreamTyping: true, PunchCapable: true,
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
	if !ack.StreamTyping {
		t.Fatal("hub did not negotiate stream typing")
	}
	if err := dialEchoThroughTunnel(ack.PublicPort); err != nil {
		t.Fatalf("echo through typed tunnel: %v", err)
	}

	// The hub has no reflector, so the relay must NOT be advertised punch-capable
	// even though the relay offered it.
	_, _, lastReq := registrar.stats()
	if lastReq.PunchCapable {
		t.Fatal("relay advertised punch_capable without a hub reflector")
	}
}

func TestRegistryCompareAndDelete(t *testing.T) {
	h := &Hub{}
	t1 := &tunnel{relayID: "r1"}
	t2 := &tunnel{relayID: "r1"}

	h.addTunnel("r1", t1)
	if got := h.lookupTunnel("r1"); got != t1 {
		t.Fatal("lookup did not return the registered tunnel")
	}
	// A stale teardown of a different tunnel instance must not evict the live one.
	h.removeTunnel("r1", t2)
	if got := h.lookupTunnel("r1"); got != t1 {
		t.Fatal("compare-and-delete evicted the live tunnel")
	}
	h.removeTunnel("r1", t1)
	if got := h.lookupTunnel("r1"); got != nil {
		t.Fatal("tunnel still present after its own teardown")
	}
}

func TestSendPunchDirectiveUnknownRelay(t *testing.T) {
	h := &Hub{}
	_, err := h.SendPunchDirective(context.Background(), "missing", punchcore.PunchDirective{})
	if err != ErrRelayNotConnected {
		t.Fatalf("err = %v, want ErrRelayNotConnected", err)
	}
}

// TestPunchLimiterPrunesIdleBuckets is the regression test for the unbounded-map
// DoS: idle buckets must be evicted so an attacker spraying fresh keys cannot
// grow the map without bound.
func TestPunchLimiterPrunesIdleBuckets(t *testing.T) {
	l := newPunchLimiter(5, 10)
	l.allow("stale-key")

	// Make the bucket look long-idle and force the next allow() to prune.
	l.mu.Lock()
	l.buckets["stale-key"].last = time.Now().Add(-2 * limiterIdleTTL)
	l.lastPrune = time.Now().Add(-2 * limiterPruneInterval)
	l.mu.Unlock()

	l.allow("fresh-key") // triggers pruneLocked

	l.mu.Lock()
	_, stale := l.buckets["stale-key"]
	_, fresh := l.buckets["fresh-key"]
	l.mu.Unlock()
	if stale {
		t.Fatal("idle bucket was not pruned (unbounded-growth DoS)")
	}
	if !fresh {
		t.Fatal("fresh bucket should be retained")
	}
}

// setupPunchHub wires an echo server + reflector + hub + coordinator + a real
// relay Client on loopback and waits for the relay to be registered and
// present in the hub registry. It returns the session context (drives the whole
// setup), the coordinator base URL, and the relay ID.
func setupPunchHub(t *testing.T, ttl time.Duration) (context.Context, string, string) {
	t.Helper()
	echoHost, echoPort := startEchoServer(t)

	reflector, err := punchcore.NewReflector(reflectorTestAddrs(), discardLogger())
	if err != nil {
		t.Fatalf("reflector: %v", err)
	}
	t.Cleanup(func() { _ = reflector.Close() })

	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	port := freePort(t)
	alloc, err := NewPortAllocator(port, port)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}
	registrar := &fakeRegistrar{relayID: "relay_punch"}
	hub := &Hub{
		ControlListener:   controlLn,
		PublicHost:        "127.0.0.1",
		PublicBindHost:    "127.0.0.1",
		Allocator:         alloc,
		Registrar:         registrar,
		HeartbeatInterval: 25 * time.Millisecond,
		HandshakeTimeout:  2 * time.Second,
		ReflectorAddrs:    reflector.Addrs(),
		Logger:            discardLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = hub.Serve(ctx) }()

	coordinator, err := NewPunchCoordinator(hub, reflector, ttl, discardLogger())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	mux := http.NewServeMux()
	coordinator.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ackCh := make(chan HelloAckFrame, 1)
	relayClient := &Client{
		HubAddr: controlLn.Addr().String(),
		Hello: HelloFrame{
			RealityPublicKey: "pk", ShortID: "sid", ServerName: "www.example.com",
			ClientID: "cid", Flow: relay.FlowVision, ExitMode: relay.ExitModeDirect,
			MaxSessions: 4, MaxMbps: 10, RelayVersion: "test",
			StreamTyping: true, PunchCapable: true,
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
	go func() { _ = relayClient.Run(ctx) }()

	select {
	case <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not register")
	}
	if !eventually(2*time.Second, func() bool { return hub.lookupTunnel("relay_punch") != nil }) {
		t.Fatal("relay not present in hub registry")
	}
	if _, _, lastReq := registrar.stats(); !lastReq.PunchCapable {
		t.Fatal("relay not advertised punch_capable")
	}
	return ctx, ts.URL, "relay_punch"
}

func establishPunch(t *testing.T, ctx context.Context, hubURL, relayID string) *punch.Establishment {
	t.Helper()
	dialer := &punch.Dialer{
		Hub:     punchcore.HubClient{BaseURL: hubURL},
		RelayID: relayID,
		Logger:  discardLogger(),
	}
	establishCtx, establishCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer establishCancel()
	est, res, err := dialer.Establish(establishCtx)
	if err != nil {
		t.Fatalf("punch establish failed: %v (result=%+v)", err, res)
	}
	t.Cleanup(func() { _ = est.Close() })
	go func() { _ = est.Bridge.Serve(ctx) }()
	return est
}

// TestHubPunchEndToEnd drives the whole coordination path on loopback: a real
// relay Client tunnels to a hub with a reflector + coordinator, then a
// punch.Dialer establishes a direct QUIC path and echoes bytes through it. This
// exercises control + discovery + punch + QUIC + bridge, though loopback is not
// real NAT.
func TestHubPunchEndToEnd(t *testing.T) {
	ctx, hubURL, relayID := setupPunchHub(t, 6*time.Second)
	est := establishPunch(t, ctx, hubURL, relayID)
	if err := dialEchoThroughTunnel(est.BridgePort); err != nil {
		t.Fatalf("echo through punched path: %v", err)
	}
}

// TestHubPunchSessionOutlivesTTL is the regression test for the lifetime bug: the
// punch TTL bounds only hole-opening, so a session established under a short TTL
// must keep working well after the TTL elapses (the relay bridge must run for
// the tunnel lifetime, not the punch budget).
func TestHubPunchSessionOutlivesTTL(t *testing.T) {
	const ttl = 700 * time.Millisecond
	ctx, hubURL, relayID := setupPunchHub(t, ttl)
	est := establishPunch(t, ctx, hubURL, relayID)

	if err := dialEchoThroughTunnel(est.BridgePort); err != nil {
		t.Fatalf("echo immediately after punch: %v", err)
	}
	// Wait well past the punch TTL, then confirm the direct path still carries a
	// fresh connection (would fail if the relay tore down at TTL).
	time.Sleep(2 * ttl)
	if err := dialEchoThroughTunnel(est.BridgePort); err != nil {
		t.Fatalf("echo after TTL elapsed (session should outlive punch TTL): %v", err)
	}
}

// reflectorTestAddrs uses two loopback IPs when available (Linux/CI), else one.
func reflectorTestAddrs() []string {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 0})
	if err != nil {
		return []string{"127.0.0.1:0"}
	}
	_ = conn.Close()
	return []string{"127.0.0.1:0", "127.0.0.2:0"}
}
