package vpnservice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/desktop/discovery"
	"openrung/internal/clienttelemetry"
	"openrung/internal/relay"
)

// telemetrySink is a loopback broker that records every telemetry event the
// service flushes (loopback endpoints are exempt from the HTTPS requirement,
// like the relay-list signing exemption).
type telemetrySink struct {
	mu     sync.Mutex
	events []clienttelemetry.Event
	seen   map[string]bool
	srv    *httptest.Server
}

func newTelemetrySink(t *testing.T) *telemetrySink {
	t.Helper()
	sink := &telemetrySink{seen: map[string]bool{}}
	sink.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Events []clienttelemetry.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
			sink.mu.Lock()
			for _, event := range batch.Events {
				// The manager is at-least-once (concurrent flushes may resend a
				// snapshot); dedupe by event id like the real broker ingest.
				if sink.seen[event.EventID] {
					continue
				}
				sink.seen[event.EventID] = true
				sink.events = append(sink.events, event)
			}
			sink.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.srv.Close)
	return sink
}

func (sink *telemetrySink) named(name string) []clienttelemetry.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []clienttelemetry.Event
	for _, event := range sink.events {
		if event.Event == name {
			out = append(out, event)
		}
	}
	return out
}

func relayAt(id, countryCode, city, country, host string) relay.Descriptor {
	r := usableRelay(id, countryCode, city, country)
	r.PublicHost = host
	return r
}

// newLadderService builds a Service with every network seam faked out; the
// returned service still runs the real ladder, telemetry manager, and state
// machine.
func newLadderService(t *testing.T, relays func() []relay.Descriptor) (*Service, *capturingEmitter) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("AppData", tmp)

	cap := &capturingEmitter{}
	s := New()
	s.Emitter = cap.emit
	s.PunchEnabled = false
	s.proxy = &fakeProxyController{supported: false}
	s.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		return discovery.Fetch{BrokerURL: brokerURL, Response: listOf(relays()...)}, nil
	}
	// Defaults every test can override: relays reachable, tunnels healthy until
	// cancelled, probes instant.
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) { return 1, nil }
	s.runTunnel = func(ctx context.Context, configPath string) error {
		<-ctx.Done()
		return nil
	}
	s.probeTunnel = func(ctx context.Context, proxyPort int) (int64, error) { return 2, nil }
	s.healthProbe = func(ctx context.Context, proxyPort int) error { return nil }
	// No real sing-box binds the loopback port in tests, so report readiness
	// immediately instead of dialing it.
	s.tunnelReady = func(ctx context.Context, proxyPort int) error { return nil }
	return s, cap
}

func waitForStatus(t *testing.T, s *Service, want ConnectionStatus) NativeVpnState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state := s.GetState()
		if state.Status == want {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %q; last state %+v", want, s.GetState())
	return NativeVpnState{}
}

// waitIdle waits for the connect goroutine to fully finish so tests never leak
// a supervisor into the next test.
func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		idle := s.conn == nil
		s.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the connection to finish")
}

func logLines(s *Service) string {
	return strings.Join(s.GetState().LogLines, "\n")
}

func TestLadderFailsOverToNextCandidate(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{
		relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
		relayAt("c", "DE", "Berlin", "Germany", "127.0.0.12"),
	}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })

	// Relay a is unreachable; relay b starts but never passes the internet
	// probe; relay c wins.
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		if host == "127.0.0.10" {
			return 0, errors.New("relay 127.0.0.10:443 is not reachable: dial tcp: i/o timeout")
		}
		return 7, nil
	}
	var probes int32
	s.probeTunnel = func(ctx context.Context, proxyPort int) (int64, error) {
		if atomic.AddInt32(&probes, 1) == 1 {
			return 0, errors.New("VPN started, but the internet probe failed: probe timeout")
		}
		return 42, nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	state := waitForStatus(t, s, StatusConnected)
	if state.RelayLabel == nil || *state.RelayLabel != "Berlin, Germany" {
		t.Fatalf("relayLabel = %v, want the winning candidate's location", state.RelayLabel)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	attempts := sink.named("relay_attempt_failed")
	if len(attempts) != 2 {
		t.Fatalf("relay_attempt_failed events = %d, want 2 (%+v)", len(attempts), attempts)
	}
	if attempts[0].RelayID != "a" || attempts[0].Measurements["attempt"] != 1 {
		t.Fatalf("first attempt event = %+v", attempts[0])
	}
	if attempts[1].RelayID != "b" || attempts[1].Measurements["attempt"] != 2 {
		t.Fatalf("second attempt event = %+v", attempts[1])
	}

	succeeded := sink.named("connection_succeeded")
	if len(succeeded) != 1 || succeeded[0].RelayID != "c" {
		t.Fatalf("connection_succeeded = %+v", succeeded)
	}
	meas := succeeded[0].Measurements
	for _, key := range []string{"broker_fetch_ms", "relay_tcp_ms", "tunnel_start_ms", "internet_probe_ms", "relay_attempts"} {
		if _, ok := meas[key]; !ok {
			t.Fatalf("connection_succeeded missing measurement %q: %+v", key, meas)
		}
	}
	if meas["relay_attempts"] != 3 {
		t.Fatalf("relay_attempts = %d, want 3", meas["relay_attempts"])
	}
	if stopped := sink.named("tunnel_stopped"); len(stopped) != 1 || stopped[0].RelayID != "c" {
		t.Fatalf("tunnel_stopped = %+v", stopped)
	}
	if failed := sink.named("connection_failed"); len(failed) != 0 {
		t.Fatalf("no connection_failed expected, got %+v", failed)
	}

	logs := logLines(s)
	for _, want := range []string{
		"trying relay a at 127.0.0.10:443",
		"relay a failed:",
		"trying relay b at 127.0.0.11:443",
		"verifying internet access through the VPN",
		"relay b failed:",
		"trying relay c at 127.0.0.12:443",
		"internet access verified in 42 ms",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log missing %q:\n%s", want, logs)
		}
	}
}

func TestLadderAllCandidatesFail(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{
		relayAt("a", "JP", "", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		return 0, errors.New("dial tcp: connection refused")
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	if state.LastError == nil || !strings.HasPrefix(*state.LastError, "All relay connection attempts failed.") {
		t.Fatalf("lastError = %v", state.LastError)
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 2 {
		t.Fatalf("relay_attempt_failed = %d, want 2", len(attempts))
	}
	failed := sink.named("connection_failed")
	if len(failed) != 1 || failed[0].Attributes["failure_stage"] != "relay_connect" {
		t.Fatalf("connection_failed = %+v", failed)
	}
	if ended := sink.named("connection_ended"); len(ended) != 1 {
		t.Fatalf("connection_ended = %+v", ended)
	}
}

func TestPinnedRelayNeverFallsBack(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{
		relayAt("a", "JP", "", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	var dialedHosts []string
	var dialMu sync.Mutex
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		dialMu.Lock()
		dialedHosts = append(dialedHosts, host)
		dialMu.Unlock()
		return 0, errors.New("dial tcp: connection refused")
	}

	if err := s.Connect(sink.srv.URL, "", "b"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	dialMu.Lock()
	defer dialMu.Unlock()
	if len(dialedHosts) != 1 || dialedHosts[0] != "127.0.0.11" {
		t.Fatalf("pinned connect dialed %v, want only the pinned relay", dialedHosts)
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 1 || attempts[0].RelayID != "b" {
		t.Fatalf("relay_attempt_failed = %+v", attempts)
	}
}

func TestDisconnectMidLadderIsNotAFailure(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{relayAt("a", "JP", "", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	dialing := make(chan struct{})
	var once sync.Once
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		return 0, ctx.Err()
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	<-dialing
	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	if failed := sink.named("connection_failed"); len(failed) != 0 {
		t.Fatalf("a user disconnect must not report connection_failed, got %+v", failed)
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 0 {
		t.Fatalf("a cancelled rung must not blame the relay, got %+v", attempts)
	}
	if ended := sink.named("connection_ended"); len(ended) != 1 || ended[0].Attributes["reason"] != "disconnect" {
		t.Fatalf("connection_ended = %+v", ended)
	}
}

func TestUnexpectedTunnelExitFailsOverMidSession(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{
		relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })

	crash := make(chan error, 1)
	var runs int32
	s.runTunnel = func(ctx context.Context, configPath string) error {
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
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	state := waitForStatus(t, s, StatusConnected)
	if state.RelayLabel == nil || *state.RelayLabel != "Tokyo, Japan" {
		t.Fatalf("initial relayLabel = %v", state.RelayLabel)
	}

	crash <- errors.New("sing-box exited: exit status 1")

	// The supervisor recovers on its own: connecting, then connected via the
	// next relay (the dead one is demoted to the end of the fresh list).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := s.GetState()
		if st.Status == StatusConnected && st.RelayLabel != nil && *st.RelayLabel == "Singapore" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	state = s.GetState()
	if state.Status != StatusConnected || state.RelayLabel == nil || *state.RelayLabel != "Singapore" {
		t.Fatalf("failover did not land on relay b: %+v", state)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	// The trigger dents relay a (no attempt measurement — not a ladder rung).
	var triggerEvents []clienttelemetry.Event
	for _, event := range sink.named("relay_attempt_failed") {
		if event.RelayID == "a" {
			triggerEvents = append(triggerEvents, event)
		}
	}
	if len(triggerEvents) != 1 || len(triggerEvents[0].Measurements) != 0 {
		t.Fatalf("failover trigger events = %+v", triggerEvents)
	}

	failovers := sink.named("relay_failover")
	if len(failovers) != 1 || failovers[0].RelayID != "b" || failovers[0].Attributes["from_relay_id"] != "a" {
		t.Fatalf("relay_failover = %+v", failovers)
	}
	// Both relays that carried the connection are credited with a
	// connection_succeeded (the broker ranks on these), so a failover winner is
	// not silently dropped from the ranking.
	succeeded := sink.named("connection_succeeded")
	if len(succeeded) != 2 || succeeded[0].RelayID != "a" || succeeded[1].RelayID != "b" {
		t.Fatalf("connection_succeeded = %+v (want the initial connect to a then the failover to b)", succeeded)
	}
	if _, ok := succeeded[1].Measurements["relay_tcp_ms"]; !ok {
		t.Fatalf("failover connection_succeeded missing measurements: %+v", succeeded[1].Measurements)
	}
	// One session throughout: no terminal events besides the final disconnect.
	if failed := sink.named("connection_failed"); len(failed) != 0 {
		t.Fatalf("mid-session failover must not end the session, got %+v", failed)
	}
	if ended := sink.named("connection_ended"); len(ended) != 1 || ended[0].Attributes["reason"] != "disconnect" {
		t.Fatalf("connection_ended = %+v", ended)
	}
}

func TestTerminalFailureRacingDisconnectNeverSticks(t *testing.T) {
	// A connect that fails on its own (all relays down) can finalize at the same
	// instant the user clicks Disconnect. The terminal status must always win —
	// the state machine must never wedge on connecting/disconnecting.
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{relayAt("a", "JP", "", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		return 0, errors.New("dial tcp: connection refused")
	}

	for i := 0; i < 60; i++ {
		if err := s.Connect(sink.srv.URL, "", ""); err != nil {
			t.Fatalf("connect: %v", err)
		}
		// Race a disconnect against the self-terminating flow.
		go func() { _ = s.Disconnect() }()
		waitIdle(t, s)
		if st := s.GetState().Status; st == StatusConnecting || st == StatusDisconnecting || st == StatusPreparing {
			t.Fatalf("iteration %d wedged on transient status %q", i, st)
		}
	}
}

func TestDisconnectDuringProbeDoesNotCommitConnected(t *testing.T) {
	// The mobile ensureActive guard: a disconnect that lands while the winning
	// candidate's internet probe is completing must NOT flash CONNECTED or
	// record connection_succeeded for a session the user ended.
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })

	probing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.probeTunnel = func(ctx context.Context, proxyPort int) (int64, error) {
		once.Do(func() { close(probing) })
		<-release // hold the probe open until the test triggers Disconnect
		return 2, nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	<-probing
	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	close(release) // probe now returns success into a disconnecting flow

	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	// No success recorded, and the state machine never latched CONNECTED.
	if succeeded := sink.named("connection_succeeded"); len(succeeded) != 0 {
		t.Fatalf("cancelled connect recorded connection_succeeded: %+v", succeeded)
	}
	if s.GetState().Status != StatusDisconnected {
		t.Fatalf("final status = %q, want disconnected", s.GetState().Status)
	}
}

func TestConcurrentConnectsLeaveOneLiveTunnel(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}

	var running int32
	build := func() *Service {
		s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
		// Each live tunnel increments a counter for its whole lifetime; an
		// orphaned connection immune to Disconnect would leave it above zero.
		s.runTunnel = func(ctx context.Context, configPath string) error {
			atomic.AddInt32(&running, 1)
			<-ctx.Done()
			atomic.AddInt32(&running, -1)
			return nil
		}
		return s
	}
	s := build()

	// Fire overlapping Connects the way a double-click on the map would.
	for i := 0; i < 30; i++ {
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = s.Connect(sink.srv.URL, "", "") }()
		}
		wg.Wait()
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	// After a single Disconnect, no tunnel goroutine may still be running: a
	// leaked orphan (the pre-fix bug) would keep one alive forever.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&running) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d tunnel(s) still running after Disconnect", atomic.LoadInt32(&running))
}

func TestPinnedDeadRelayFailsOverViaNetworkGate(t *testing.T) {
	// A pinned single relay whose host dies mid-session: sing-box keeps running
	// (never exits), so only the health monitor can notice. With the network
	// alive, the gate must still let recovery run instead of wedging in a
	// permanent zombie-connected state.
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.healthTick = 5 * time.Millisecond

	// The broker/telemetry sink is the network-alive reference; it is listening,
	// so the gate sees a live network. Health probes fail from the start.
	s.healthProbe = func(ctx context.Context, proxyPort int) error {
		return errors.New("probe timeout")
	}

	if err := s.Connect(sink.srv.URL, "", "a"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	// The health monitor should trigger a recovery re-ladder (relay a is the
	// only candidate, demoted then retried) rather than declaring a local
	// outage against the single dead relay. Recovery re-promotes relay a.
	deadline := time.Now().Add(10 * time.Second)
	var failovers int
	for time.Now().Before(deadline) {
		if failovers = len(sink.named("relay_failover")); failovers >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failovers < 1 {
		t.Fatal("health monitor never failed over for a pinned dead relay with a live network")
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// liveFront returns a listening loopback address (network-alive signal) and a
// closed loopback address (network-down: connection refused, fails fast).
func liveFront(t *testing.T) (live, dead string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := closed.Addr().String()
	closed.Close() // free the port so a dial is refused
	return listener.Addr().String(), deadAddr
}

func TestHealthLoopGateRequiresLiveNetwork(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "", "Japan", "127.0.0.10")}
	live, dead := liveFront(t)
	newMonitorService := func() *Service {
		s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
		s.healthTick = 5 * time.Millisecond
		s.healthProbe = func(ctx context.Context, proxyPort int) error {
			return errors.New("probe timeout")
		}
		return s
	}

	// Network down (no front answers): the monitor must NOT fail over however
	// many probes fail — it can't tell a dead tunnel from a local outage.
	offline := newMonitorService()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failCh := make(chan error, 1)
	go offline.healthLoop(ctx, 1080, []string{dead}, failCh)
	select {
	case err := <-failCh:
		t.Fatalf("health loop failed over during a local outage: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Network alive (a front answers): the same failing probe now triggers.
	online := newMonitorService()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	failCh2 := make(chan error, 1)
	go online.healthLoop(ctx2, 1080, []string{dead, live}, failCh2)
	select {
	case err := <-failCh2:
		if !strings.Contains(err.Error(), "health probe failed") {
			t.Fatalf("unexpected trigger error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("health loop never triggered with a live network")
	}
}

func TestHealthLoopResetsOnProbeSuccess(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "", "Japan", "127.0.0.10")}
	live, _ := liveFront(t)
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.healthTick = 5 * time.Millisecond
	// Fail twice, succeed, repeat: the consecutive counter must never reach the
	// threshold of 3.
	var calls int32
	s.healthProbe = func(ctx context.Context, proxyPort int) error {
		if atomic.AddInt32(&calls, 1)%3 == 0 {
			return nil
		}
		return errors.New("probe timeout")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failCh := make(chan error, 1)
	go s.healthLoop(ctx, 1080, []string{live}, failCh)
	select {
	case err := <-failCh:
		t.Fatalf("health loop triggered despite periodic probe successes: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
