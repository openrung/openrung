package connectcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"
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
	if !strings.Contains(logLines(s), "local VPN setup failed at socket_protection") {
		t.Fatalf("refusal was not classified as a local failure:\n%s", logLines(s))
	}
}

// A protection refusal on the ladder dial is a LOCAL platform failure: it
// must not dent the relay's broker health and must not unlock the relay's
// WSS fronts — retrying a relay or minting a ticket cannot repair a host
// that refuses to protect sockets.
func TestSocketProtectionRefusalIsLocalNotRelayEvidence(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, fmt.Errorf("relay 127.0.0.10:443 is not reachable: %w", errSocketProtectionFailed)
	}
	var tickets atomic.Int32
	s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
		tickets.Add(1)
		return brokerapi.WSSTicketResponse{}, errors.New("must not be reached")
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if state.LastError == nil || !strings.Contains(*state.LastError, "socket protection failed") {
		t.Fatalf("lastError = %v", state.LastError)
	}
	if tickets.Load() != 0 {
		t.Fatalf("a local protection refusal unlocked WSS fallback (%d tickets)", tickets.Load())
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 0 {
		t.Fatalf("a local protection refusal dented relay health: %+v", attempts)
	}
	if !strings.Contains(logLines(s), "local VPN setup failed at socket_protection") {
		t.Fatalf("refusal was not classified as a local failure:\n%s", logLines(s))
	}
}

// A protection refusal during the WSS TICKET fetch is the same local failure:
// it must stop after the first broker-front attempt — not grind through every
// front and the bounded retry round — and must not be recorded as front
// damage.
func TestWSSTicketProtectionRefusalStopsAfterOneAttempt(t *testing.T) {
	sink := newTelemetrySink(t)
	frontA := testWSSFront("front-a", testWSSFrontAURL)
	frontB := testWSSFront("front-b", testWSSFrontBURL)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10", frontA, frontB)
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("direct TCP blocked") // genuinely unlocks the fronts
	}
	var tickets atomic.Int32
	s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
		tickets.Add(1)
		return brokerapi.WSSTicketResponse{}, fmt.Errorf("post ticket request: %w", errSocketProtectionFailed)
	}
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		t.Error("a refused ticket fetch still dialed a WSS front")
		return nil, errors.New("unreachable")
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if state.LastError == nil || !strings.Contains(*state.LastError, "socket protection failed") {
		t.Fatalf("lastError = %v", state.LastError)
	}
	if tickets.Load() != 1 {
		t.Fatalf("ticket attempts = %d; a local refusal must stop after the first broker front", tickets.Load())
	}
	if failures := sink.named("transport_failed"); len(failures) != 0 {
		t.Fatalf("a local protection refusal was recorded as front damage: %+v", failures)
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 1 {
		t.Fatalf("relay_attempt_failed = %+v; want exactly the genuine direct failure", attempts)
	}
	if !strings.Contains(logLines(s), "local VPN setup failed at socket_protection") {
		t.Fatalf("refusal was not classified as a local failure:\n%s", logLines(s))
	}
}

// The same classification on the WSS path: wsscore's protection sentinel from
// the bridge dial stops the ladder instead of burning the remaining fronts'
// single-use tickets or recording transport damage.
func TestWSSDialProtectionRefusalStopsLadderWithoutFrontDamage(t *testing.T) {
	sink := newTelemetrySink(t)
	frontA := testWSSFront("front-a", testWSSFrontAURL)
	frontB := testWSSFront("front-b", testWSSFrontBURL)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10", frontA, frontB)
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("direct TCP blocked") // genuinely unlocks the fronts
	}
	var tickets atomic.Int32
	s.requestWSSTicket = func(_ context.Context, _ string, request brokerapi.WSSTicketRequest, _, _ string) (brokerapi.WSSTicketResponse, error) {
		tickets.Add(1)
		if request.FrontID == frontA.ID {
			return successfulWSSTicket(frontA, "single-use"), nil
		}
		return successfulWSSTicket(frontB, "single-use"), nil
	}
	var wssDials atomic.Int32
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		wssDials.Add(1)
		return nil, fmt.Errorf("connect WSS front: %w", wsscore.ErrSocketProtectionFailed)
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if wssDials.Load() != 1 || tickets.Load() != 1 {
		t.Fatalf("refusal did not stop the ladder: dials=%d tickets=%d (front-b's ticket burned for nothing)", wssDials.Load(), tickets.Load())
	}
	if failures := sink.named("transport_failed"); len(failures) != 0 {
		t.Fatalf("a local protection refusal was recorded as front damage: %+v", failures)
	}
	// The direct failure that unlocked the fronts stays legitimately recorded.
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 1 {
		t.Fatalf("relay_attempt_failed = %+v; want exactly the genuine direct failure", attempts)
	}
}
