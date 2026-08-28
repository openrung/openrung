package connectcore

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// recordingProtector is a fake VpnService.protect: it remembers every fd the
// engine asked it to protect and can refuse, the way Android does when the
// VPN service is gone.
type recordingProtector struct {
	mu     sync.Mutex
	fds    []int32
	refuse bool
}

func (p *recordingProtector) Protect(fd int32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fds = append(p.fds, fd)
	return !p.refuse
}

func (p *recordingProtector) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.fds)
}

// protectedEngine builds a ladder-test engine whose relay dial and telemetry
// paths run against real loopback sockets through the DEFAULT (protected)
// constructions, with a live loopback "relay" listener standing in for the
// relay's public endpoint.
func protectedEngine(t *testing.T, protector *recordingProtector) (*Engine, *telemetrySink) {
	t.Helper()
	relayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relayListener.Close() })
	go func() {
		for {
			conn, acceptErr := relayListener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	orig := lookupGeoAttributes
	lookupGeoAttributes = func(context.Context, *http.Client) map[string]string { return nil }
	t.Cleanup(func() { lookupGeoAttributes = orig })

	sink := newTelemetrySink(t)
	fixture := relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.1")
	fixture.PublicPort = relayListener.Addr().(*net.TCPAddr).Port
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.SocketProtector = protector
	// Restore the default relay dialer: the protected dial is what this test
	// observes, against the loopback relay listener above.
	s.dialRelay = nil
	return s, sink
}

// The A2 socket-control acceptance: with a protector installed, the engine's
// own physical-network sockets — the ladder's relay reachability dial and the
// telemetry upload here, both real loopback connections — are handed to
// Protect before they connect, and the connect still succeeds.
func TestSocketProtectorObservedOnLoopbackConnect(t *testing.T) {
	protector := &recordingProtector{}
	s, sink := protectedEngine(t, protector)

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	// The success flush is the telemetry POST; wait for it to land so both
	// socket classes (raw relay dial, broker HTTP) have been protected.
	deadline := time.Now().Add(10 * time.Second)
	for len(sink.named("connection_succeeded")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("connection_succeeded never reached the loopback broker")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := protector.count(); got < 2 {
		t.Fatalf("protector observed %d socket(s); want at least the relay dial and the telemetry upload", got)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// A refused protection must fail the dial closed — an unprotected socket
// would not error on its own, it would silently route into the tunnel — so
// the connect fails naming the protection, and no relay is blamed as
// unreachable-by-network.
func TestSocketProtectorRefusalFailsClosed(t *testing.T) {
	protector := &recordingProtector{refuse: true}
	s, sink := protectedEngine(t, protector)

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if state.LastError == nil || !strings.Contains(*state.LastError, "socket protection failed") {
		t.Fatalf("lastError = %v; want the socket-protection failure", state.LastError)
	}
	if protector.count() == 0 {
		t.Fatal("the refusing protector was never consulted")
	}
}
