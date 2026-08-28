package connectcore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// The A2 lifecycle acceptance: Pause silences the engine's periodic activity
// — health sweeps and telemetry heartbeats — and Resume runs what the pause
// held right away.
func TestPauseHoldsHealthSweepsAndHeartbeatsUntilResume(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.healthTick = 15 * time.Millisecond
	s.heartbeatTick = 15 * time.Millisecond
	var probes atomic.Int32
	s.healthProbe = func(context.Context, int) error {
		probes.Add(1)
		return nil
	}
	heartbeats := func() int { return len(sink.named("session_heartbeat")) }

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	deadline := time.Now().Add(10 * time.Second)
	for probes.Load() < 2 || heartbeats() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("periodic machinery never ran: probes=%d heartbeats=%d", probes.Load(), heartbeats())
		}
		time.Sleep(2 * time.Millisecond)
	}

	s.Pause()
	s.Pause()                          // idempotent
	time.Sleep(100 * time.Millisecond) // let anything already past the gate finish
	pausedProbes, pausedHeartbeats := probes.Load(), heartbeats()
	time.Sleep(150 * time.Millisecond)
	if got := probes.Load(); got != pausedProbes {
		t.Fatalf("health sweeps kept running while paused: %d -> %d", pausedProbes, got)
	}
	if got := heartbeats(); got != pausedHeartbeats {
		t.Fatalf("heartbeats kept uploading while paused: %d -> %d", pausedHeartbeats, got)
	}

	s.Resume()
	s.Resume() // idempotent
	deadline = time.Now().Add(10 * time.Second)
	for probes.Load() <= pausedProbes || heartbeats() <= pausedHeartbeats {
		if time.Now().After(deadline) {
			t.Fatalf("periodic machinery never resumed: probes=%d heartbeats=%d", probes.Load(), heartbeats())
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// A failover trigger that fires while paused is held, not lost: the session
// stays visibly CONNECTED (no recovery network activity starts), and Resume
// runs the recovery immediately.
func TestPausedEngineHoldsFailoverUntilResume(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{
		relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	crash := make(chan error, 1)
	var runs int32
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, configJSON []byte) error {
		if atomic.AddInt32(&runs, 1) == 1 {
			select {
			case err := <-crash:
				return err
			case <-ctx.Done():
				return nil
			}
		}
		<-ctx.Done()
		return nil
	})

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)

	s.Pause()
	crash <- errors.New("tunnel died while the device slept")
	time.Sleep(120 * time.Millisecond)
	if got := events.noticesOf(NoticeFailoverCompleted); len(got) != 0 {
		t.Fatalf("recovery ran while paused: %+v", got)
	}
	if state := s.State(); state.Status != StatusConnected {
		t.Fatalf("status while paused = %s; the held trigger should not surface yet", state.Status)
	}

	s.Resume()
	deadline := time.Now().Add(10 * time.Second)
	for len(events.noticesOf(NoticeFailoverCompleted)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the held failover never ran after Resume")
		}
		time.Sleep(2 * time.Millisecond)
	}
	waitForStatus(t, s, StatusConnected)
	if atomic.LoadInt32(&runs) != 2 {
		t.Fatalf("runs = %d; want the crashed run plus its recovery", runs)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// The seams composed: a network epoch that lands while paused is held with
// the session intact, and Resume rebuilds the WSS session on the new epoch —
// the device-sleep-then-wake sequence the mobile adapters will drive.
func TestNetworkEpochWhilePausedRebuildsAfterResume(t *testing.T) {
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

	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi-home"}) // baseline
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	waitWSSSignal(t, first.started, "first WSS session")

	s.Pause()
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cellular"}) // epoch while asleep
	time.Sleep(120 * time.Millisecond)
	if bridgeCalls.Load() != 1 {
		t.Fatal("the paused engine rebuilt the WSS session")
	}
	if state := s.State(); state.Status != StatusConnected {
		t.Fatalf("status while paused = %s", state.Status)
	}

	s.Resume()
	waitWSSSignal(t, second.started, "post-resume rebuild on the new epoch")
	waitForStatus(t, s, StatusConnected)
	if got := events.noticesOf(NoticeFailoverStarted); len(got) != 1 || got[0].Reason != "physical network epoch changed" {
		t.Fatalf("failover notice = %+v; want exactly the held epoch", got)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// Shutdown's ordering guarantee: the tunnel is stopped and the terminal state
// emitted before it returns, with the telemetry flush bounded by the caller's
// budget and its outcome reported — against a broker that hangs, Shutdown
// returns at the budget with the flush error instead of hanging with it.
func TestShutdownBoundsTheTerminalFlushAndReportsOutcome(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold every upload until the client gives up (the release only
		// unblocks Server.Close at test end — a parked HTTP/1 handler does
		// not observe the client's abort).
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hanging.Close()
	defer close(release)

	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	if err := s.Connect(hanging.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)

	started := time.Now()
	err := s.Shutdown(100 * time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Shutdown reported a clean flush against a hanging broker")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v; the flush budget did not bound it", elapsed)
	}
	if state := s.State(); state.Status != StatusDisconnected {
		t.Fatalf("status after Shutdown = %s; the terminal state must precede the return", state.Status)
	}
	waitIdle(t, s)

	// Idle Shutdown is a cheap nil.
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("idle Shutdown = %v", err)
	}
}

// The happy path: a responsive broker gets the session-end events inside the
// budget and Shutdown reports the clean flush.
func TestShutdownFlushesSessionEndWithinBudget(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)

	if err := s.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown flush failed: %v", err)
	}
	if ended := sink.named("connection_ended"); len(ended) != 1 || ended[0].Attributes["reason"] != "disconnect" {
		t.Fatalf("connection_ended after Shutdown = %+v", ended)
	}
	if stopped := sink.named("tunnel_stopped"); len(stopped) != 1 {
		t.Fatalf("tunnel_stopped after Shutdown = %+v", stopped)
	}
	waitIdle(t, s)
}

// Pause never blocks teardown: Shutdown while paused still stops the tunnel,
// emits the terminal state, and flushes.
func TestShutdownWorksWhilePaused(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)

	s.Pause()
	if err := s.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown while paused: %v", err)
	}
	if state := s.State(); state.Status != StatusDisconnected {
		t.Fatalf("status after paused Shutdown = %s", state.Status)
	}
	waitIdle(t, s)
	s.Resume() // leave the engine running for symmetry; must not panic
}
