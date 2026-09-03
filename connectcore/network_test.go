package connectcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// The tracker's epoch semantics, ported from the mobile monitors: the first
// observation is an absorbed baseline, identical observations are ignored,
// any change is one epoch boundary, and signals with no live session only
// advance the tracker.
func TestNetworkSignalsBaselineDedupAndIdleSafety(t *testing.T) {
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })

	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-a"})
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-a"})
	if got := events.noticesOf(NoticeNetworkEpoch); len(got) != 0 {
		t.Fatalf("baseline or duplicate produced epochs: %+v", got)
	}
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-b"})
	if got := events.noticesOf(NoticeNetworkEpoch); len(got) != 1 {
		t.Fatalf("one fingerprint change should be exactly one epoch, got %+v", got)
	}
	s.UpdateNetworkState(NetworkState{Up: false, Fingerprint: "wifi-b"})
	got := events.noticesOf(NoticeNetworkEpoch)
	if len(got) != 2 || got[1].Reason != "physical network lost" {
		t.Fatalf("an Up flip should be an epoch with the loss reason, got %+v", got)
	}
	// Idle-engine and post-session signals are tracker-only: no status change,
	// no panic.
	if state := s.State(); state.Status != StatusDisconnected {
		t.Fatalf("idle signals changed engine status to %s", state.Status)
	}
}

// A live WSS session is bound to one physical-network epoch (the mobile
// invariant): an epoch boundary retires it — orderly, blaming neither the
// relay nor the front — and recovery reruns the fresh direct-first ladder
// with a new single-use ticket.
func TestNetworkEpochRetiresWSSSessionAndRecoversDirectFirst(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.networkRetryDelay = 2 * time.Millisecond
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	var directCalls atomic.Int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		directCalls.Add(1)
		return 0, errors.New("direct path blocked")
	}
	var ticketCalls atomic.Int32
	s.requestWSSTicket = func(_ context.Context, _ string, request brokerapi.WSSTicketRequest, _, _ string) (brokerapi.WSSTicketResponse, error) {
		call := ticketCalls.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-"+string(rune('0'+call))), nil
	}
	first, second := newFakeWSSBridge(), newFakeWSSBridge()
	var bridgeCalls atomic.Int32
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		if bridgeCalls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	waitWSSSignal(t, first.started, "first WSS session")

	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-home"}) // baseline, absorbed
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cellular"})  // epoch boundary
	waitWSSSignal(t, second.started, "replacement WSS session after the epoch")
	waitForStatus(t, s, StatusConnected)
	if directCalls.Load() != 2 || ticketCalls.Load() != 2 {
		t.Fatalf("recovery was not a fresh direct-first ladder: direct=%d tickets=%d", directCalls.Load(), ticketCalls.Load())
	}
	started := events.noticesOf(NoticeFailoverStarted)
	if len(started) != 1 || started[0].Reason != "physical network epoch changed" {
		t.Fatalf("failover notice = %+v; want the epoch reason", started)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	if failures := sink.named("transport_failed"); len(failures) != 0 {
		t.Fatalf("an epoch retirement was reported as path loss: %+v", failures)
	}
	ended := sink.named("transport_session_ended")
	if len(ended) != 1 || ended[0].Attributes["trigger"] != "network_epoch" {
		t.Fatalf("epoch retirement telemetry = %+v; want transport_session_ended with the network_epoch trigger", ended)
	}
	for _, attempt := range sink.named("relay_attempt_failed") {
		if len(attempt.Measurements) == 0 {
			t.Fatalf("epoch retirement damaged relay health: %+v", attempt)
		}
	}
}

// A direct path may survive a network change, so an epoch does not retire it:
// the engine runs one immediate health sweep instead — the signal accelerates
// the engine's own probing, which stays the authority.
func TestNetworkEpochKicksImmediateHealthProbeOnDirectPath(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.healthTick = time.Hour // only a kick can produce a sweep in test time
	var probes atomic.Int32
	s.healthProbe = func(context.Context, int) error {
		probes.Add(1)
		return nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-home"})
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cellular"})

	deadline := time.Now().Add(5 * time.Second)
	for probes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the epoch never kicked an immediate health sweep")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := events.noticesOf(NoticeFailoverStarted); len(got) != 0 {
		t.Fatalf("a healthy direct path was retired by the epoch: %+v", got)
	}
	if state := s.State(); state.Status != StatusConnected {
		t.Fatalf("status = %s; the healthy session should have survived the epoch", state.Status)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// The epoch baseline is the CANDIDATE's, captured before its attempt dials
// anything: epochs that predate the attempt never retire the session, while
// one that lands between the transport's dial and promote is still pending
// afterwards and rebuilds the fresh session — its sockets may be bound to the
// network that just died (a wifi-to-cellular handover mid-connect must not be
// absorbed into the baseline).
func TestEpochBetweenDialAndPromoteRetiresTheFreshSession(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.networkRetryDelay = 2 * time.Millisecond
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("direct path blocked")
	}
	var ticketCalls atomic.Int32
	s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
		call := ticketCalls.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-"+string(rune('0'+call))), nil
	}
	first, second := newFakeWSSBridge(), newFakeWSSBridge()
	var bridgeCalls atomic.Int32
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		if bridgeCalls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	// Hold the first candidate at readiness so an epoch can land mid-attempt,
	// after its WSS socket exists but before promote.
	readyGate, release := holdReadiness(s)

	// Epochs BEFORE the attempt are settled history: baseline plus a change,
	// both pre-connect, must not retire the session that follows.
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-home"})
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-office"})
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	<-readyGate
	// The handover lands mid-attempt: after the WSS dial, before promote.
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cellular"})
	release()

	// The fresh session is retired for the epoch its socket predates and
	// rebuilt on the new one.
	waitWSSSignal(t, second.started, "rebuild after the mid-attempt handover")
	waitForStatus(t, s, StatusConnected)
	if ticketCalls.Load() != 2 {
		t.Fatalf("ticket calls = %d; want the held session plus its rebuild", ticketCalls.Load())
	}
	started := events.noticesOf(NoticeFailoverStarted)
	if len(started) != 1 || started[0].Reason != "physical network epoch changed" {
		t.Fatalf("failover notices = %+v; want exactly the mid-attempt handover (pre-attempt epochs must stay absorbed)", started)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// During a network outage, a platform signal wakes the recovery gate
// immediately instead of waiting out its poll interval — but the dial probe
// stays the authority on "alive", so the signal accelerates recovery and can
// never wedge it.
func TestNetworkSignalWakesRecoveryGateImmediately(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	// Only a signal can wake the gate in test time.
	s.networkRetryDelay = time.Hour
	var networkUp atomic.Bool
	networkUp.Store(true)
	s.checkNetworkAlive = func(context.Context, []string) bool { return networkUp.Load() }

	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("direct path blocked")
	}
	var ticketCalls atomic.Int32
	s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
		call := ticketCalls.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-"+string(rune('0'+call))), nil
	}
	first, second := newFakeWSSBridge(), newFakeWSSBridge()
	var bridgeCalls atomic.Int32
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		if bridgeCalls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-home"}) // baseline
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	waitWSSSignal(t, first.started, "first WSS session")

	networkUp.Store(false)
	first.fatal <- errors.New("WSS session lost with the network")
	waitForStatus(t, s, StatusConnecting)
	time.Sleep(25 * time.Millisecond) // the gate is now waiting on its hour-long poll

	networkUp.Store(true)
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cellular"})
	waitWSSSignal(t, second.started, "recovery woken by the network signal")
	waitForStatus(t, s, StatusConnected)

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}
