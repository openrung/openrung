package connectcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"
)

// The unit tests below port the mobile breaker's own suites
// (PunchRecoveryCircuitBreakerTest.kt / PunchRecoveryCircuitBreakerTests.swift)
// against the Go port; each names the behavior's platform source.

func minJitter(min, _ time.Duration) time.Duration { return min }

func testBreaker(config punchBreakerConfig) *punchRecoveryBreaker {
	return &punchRecoveryBreaker{config: config}
}

// The backoff ladder (PunchRecoveryCircuitBreaker.kt:126-139 / .swift:183-204):
// doubling toward the cap, jumping to it once past half, jittered ±20% — the
// platform tests' 100/250 ladder is [80,160,200,200,200] with the jitter
// picker pinned to the window's minimum.
func TestPunchBreakerBackoffLadderMatchesTheMobileTests(t *testing.T) {
	breaker := testBreaker(punchBreakerConfig{
		stablePath:        punchStablePath,
		maxRapid:          10, // keep the circuit closed so all five rungs are observable
		initialBackoff:    100 * time.Millisecond,
		maxBackoff:        250 * time.Millisecond,
		pickJitteredDelay: minJitter,
	})
	now := time.Now()
	want := []time.Duration{
		80 * time.Millisecond,
		160 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond,
	}
	for i, expected := range want {
		decision := breaker.onDirectPathLost("relay-a", now, true)
		if decision.delay != expected {
			t.Fatalf("failure %d delay = %v, want %v (the .kt/.swift ladder test values)", i+1, decision.delay, expected)
		}
	}
}

// Default-constant ladder windows (2s/4s/8s nominals, ±20%): failure 1 spans
// [1.6s, 2.4s] — the .swift clamp test's bounds.
func TestPunchBreakerDefaultLadderWindows(t *testing.T) {
	var pickedMin, pickedMax time.Duration
	breaker := testBreaker(punchBreakerConfig{
		pickJitteredDelay: func(min, max time.Duration) time.Duration {
			pickedMin, pickedMax = min, max
			return min
		},
	})
	breaker.onDirectPathLost("relay-a", time.Now(), true)
	if pickedMin != 1600*time.Millisecond || pickedMax != 2400*time.Millisecond {
		t.Fatalf("first failure window = [%v, %v], want [1.6s, 2.4s]", pickedMin, pickedMax)
	}
}

// A path that survived the stable threshold resets the streak BEFORE the
// current loss is counted — and the boundary is inclusive: exactly the
// threshold qualifies, one instant less does not (.kt test:50 / .swift
// test:42 pin STABLE_PATH_MS-1 as not enough).
func TestPunchBreakerStablePathResetBoundary(t *testing.T) {
	breaker := testBreaker(punchBreakerConfig{pickJitteredDelay: minJitter})
	start := time.Now()

	// Two rapid failures build a streak.
	breaker.markDirectConnected("relay-a", start)
	breaker.onDirectPathLost("relay-a", start.Add(time.Second), true)
	breaker.markDirectConnected("relay-a", start)
	if d := breaker.onDirectPathLost("relay-a", start.Add(time.Second), true); d.rapidFailures != 2 {
		t.Fatalf("streak = %d, want 2", d.rapidFailures)
	}

	// A loss after one instant less than the threshold keeps the streak.
	breaker.markDirectConnected("relay-a", start)
	almost := breaker.onDirectPathLost("relay-a", start.Add(punchStablePath-time.Nanosecond), true)
	if almost.rapidFailures != 3 || !almost.useRelayHub {
		t.Fatalf("sub-threshold uptime must count (and open at 3): %+v", almost)
	}

	// A loss after exactly the threshold resets the streak (and re-closes the
	// circuit) before counting itself as failure #1.
	breaker.markDirectConnected("relay-b", start)
	breaker.onDirectPathLost("relay-b", start.Add(time.Second), true)
	breaker.markDirectConnected("relay-b", start)
	stable := breaker.onDirectPathLost("relay-b", start.Add(punchStablePath), true)
	if stable.rapidFailures != 1 || stable.useRelayHub {
		t.Fatalf("threshold uptime must reset the streak first: %+v", stable)
	}
}

// An exempt loss (the physical network itself was down) changes nothing:
// no count, no delay — and the stable-path reset still applies to it
// (.kt test:94-108 / .swift test:93-108).
func TestPunchBreakerExemptLossCountsNothingButStillResets(t *testing.T) {
	breaker := testBreaker(punchBreakerConfig{pickJitteredDelay: minJitter})
	start := time.Now()
	breaker.markDirectConnected("relay-a", start)
	breaker.onDirectPathLost("relay-a", start.Add(time.Second), true)

	breaker.markDirectConnected("relay-a", start)
	exempt := breaker.onDirectPathLost("relay-a", start.Add(time.Second), false)
	if exempt.counted || exempt.delay != 0 || exempt.rapidFailures != 1 {
		t.Fatalf("exempt loss must keep the streak untouched with no delay: %+v", exempt)
	}

	// An exempt loss after a stable path still performs the reset.
	breaker.markDirectConnected("relay-a", start)
	reset := breaker.onDirectPathLost("relay-a", start.Add(punchStablePath), false)
	if reset.rapidFailures != 0 {
		t.Fatalf("stable-path reset must apply to an exempt loss too: %+v", reset)
	}
}

// The circuit opens at the third counted rapid failure (>=, .kt:102-111 /
// .swift:156-167), still carrying that failure's backoff; state is per-relay,
// and an open circuit refuses punches until a fresh breaker (the mobile
// reset() on a user connect).
func TestPunchBreakerOpensAtThreeRapidFailuresPerRelay(t *testing.T) {
	breaker := testBreaker(punchBreakerConfig{pickJitteredDelay: minJitter})
	now := time.Now()
	for i := 0; i < 2; i++ {
		if d := breaker.onDirectPathLost("relay-a", now, true); d.useRelayHub {
			t.Fatalf("circuit opened early at failure %d", i+1)
		}
		if !breaker.allowsDirectPunch("relay-a") {
			t.Fatal("circuit must stay closed below the threshold")
		}
	}
	third := breaker.onDirectPathLost("relay-a", now, true)
	if !third.useRelayHub || third.delay <= 0 || third.rapidFailures != 3 {
		t.Fatalf("third rapid failure must open the circuit with its backoff: %+v", third)
	}
	if breaker.allowsDirectPunch("relay-a") {
		t.Fatal("an open circuit must refuse punches")
	}
	if !breaker.allowsDirectPunch("relay-b") {
		t.Fatal("the circuit is per-relay: relay-b must stay allowed")
	}
	// A monotonic-clock regression reads as zero uptime, never negative
	// (.swift testMonotonicClockRegressionProducesZeroUptime).
	breaker.markDirectConnected("relay-c", now)
	regressed := breaker.onDirectPathLost("relay-c", now.Add(-time.Minute), true)
	if regressed.directUptime != 0 {
		t.Fatalf("clock regression uptime = %v, want 0", regressed.directUptime)
	}
}

// fakePunchBridge is a live punched path's bridge for engine-level tests.
type fakePunchBridge struct{}

func (fakePunchBridge) Serve(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (fakePunchBridge) Close() error                    { return nil }

// The engine-level acceptance: a punched path that keeps collapsing right
// after promotion opens the relay's circuit — the third rapid failure records
// punch_fallback with the mobile measurements, and every later attempt for
// the relay skips the punch (punch_skipped, reason recovery_circuit_open) and
// connects over the hub instead (OpenRungVpnService.kt:1252-1301 /
// PacketTunnelProvider.swift:1258-1297).
func TestPunchFlappingOpensTheRecoveryCircuitAndFallsBackToTheHub(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	fixture.PunchCapable = true
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.PunchEnabled = true
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }
	s.punchBreakerConfig = punchBreakerConfig{
		initialBackoff:    time.Millisecond,
		maxBackoff:        2 * time.Millisecond,
		pickJitteredDelay: minJitter,
	}
	s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
		return &PunchPath{
			BridgeHost: "127.0.0.1", BridgePort: 45111, PeerIP: "203.0.113.7",
			NATClass: "eim", Bridge: fakePunchBridge{},
		}, punchcore.PunchResult{NATClass: "eim"}, nil
	}
	crash := make(chan error)
	var runs atomic.Int32
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, _ []byte) error {
		runs.Add(1)
		select {
		case err := <-crash:
			return err
		case <-ctx.Done():
			return nil
		}
	})

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	// waitForPromotion waits until run number wantRun is the PROMOTED path of
	// the given transport — gating on the run counter keeps the next crash
	// from landing in a run that is still starting up.
	waitForPromotion := func(want string, wantRun int32) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if info, ok := s.ActiveConnectionInfo(); ok && info.Transport == want && runs.Load() == wantRun {
				return
			}
			if time.Now().After(deadline) {
				info, ok := s.ActiveConnectionInfo()
				t.Fatalf("timed out waiting for promoted %s run %d (transport=%q ok=%t runs=%d status=%s)\nlog:\n%s",
					want, wantRun, info.Transport, ok, runs.Load(), s.State().Status, logLines(s))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Three rapid collapses of the punched path: each recovery re-punches
	// (with the breaker's escalating backoff) until the third opens the
	// circuit.
	for i := int32(0); i < 3; i++ {
		waitForPromotion("punch", i+1)
		crash <- errors.New("punched path collapsed")
	}
	// The opened circuit skips the punch: the recovery lands on the hub path.
	waitForPromotion(brokerapi.TransportDirect, 4)

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	if got := runs.Load(); got != 4 {
		t.Fatalf("runs = %d; want 3 punched attempts plus the hub recovery", got)
	}
	fallbacks := sink.named("punch_fallback")
	if len(fallbacks) != 1 {
		t.Fatalf("punch_fallback events = %+v; want exactly the circuit-opening one", fallbacks)
	}
	if fallbacks[0].Attributes["failure_reason"] != "unstable_direct_path" ||
		fallbacks[0].Measurements["rapid_failure_count"] != 3 {
		t.Fatalf("punch_fallback payload = %+v; want the mobile attributes and measurements", fallbacks[0])
	}
	if _, ok := fallbacks[0].Measurements["recovery_delay_ms"]; !ok {
		t.Fatalf("punch_fallback lost the recovery delay measurement: %+v", fallbacks[0].Measurements)
	}
	skips := sink.named("punch_skipped")
	if len(skips) != 1 || skips[0].Attributes["reason"] != "recovery_circuit_open" {
		t.Fatalf("punch_skipped = %+v; want one skip with reason recovery_circuit_open", skips)
	}
	if attempts := sink.named("punch_attempted"); len(attempts) != 3 {
		t.Fatalf("punch_attempted = %d; want the three attempts before the circuit opened", len(attempts))
	}
}

// The finding-1 regression: in TUN mode the health loop deliberately skips
// its own network-alive gate, so a health-triggered punched-path loss must be
// probed HERE — a physical network outage is an exempt loss (Android's
// countTowardBreaker=false when physicalNetworkAlive() fails), never punch
// instability, and the circuit must not open however many times it repeats.
func TestTUNHealthLossesDuringNetworkOutageAreExemptFromThePunchBreaker(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	fixture.PunchCapable = true
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fixture} })
	s.Elevation = permitElevation{}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.PunchEnabled = true
	s.healthTick = 5 * time.Millisecond
	s.networkRetryDelay = 2 * time.Millisecond
	s.punchBreakerConfig = punchBreakerConfig{
		initialBackoff:    time.Millisecond,
		maxBackoff:        2 * time.Millisecond,
		pickJitteredDelay: minJitter,
	}
	s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
		return &PunchPath{
			BridgeHost: "127.0.0.1", BridgePort: 45112, PeerIP: "203.0.113.7",
			NATClass: "eim", Bridge: fakePunchBridge{},
		}, punchcore.PunchResult{NATClass: "eim"}, nil
	}
	// The physical network is down for the whole flapping phase: the exempt
	// decision (first networkAlive call of each recovery) sees it down; the
	// recovery gate's later calls see it back so the re-ladder proceeds.
	var aliveCalls atomic.Int32
	var outageOver atomic.Bool
	s.checkNetworkAlive = func(context.Context, []string) bool {
		if outageOver.Load() {
			return true
		}
		return aliveCalls.Add(1)%2 == 0 // decision call down, recovery-gate call up
	}
	var probesFailing atomic.Bool
	probesFailing.Store(true)
	s.healthProbe = func(context.Context, int) error {
		if probesFailing.Load() {
			return errors.New("through-tunnel probe blackholed")
		}
		return nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	// Well past the three losses that would open the circuit if they counted.
	deadline := time.Now().Add(10 * time.Second)
	for len(sink.named("punch_attempted")) < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("punch attempts = %d; recoveries stalled\nlog:\n%s", len(sink.named("punch_attempted")), logLines(s))
		}
		time.Sleep(2 * time.Millisecond)
	}
	probesFailing.Store(false)
	outageOver.Store(true)

	if skips := sink.named("punch_skipped"); len(skips) != 0 {
		t.Fatalf("exempt losses opened the circuit: %+v", skips)
	}
	if fallbacks := sink.named("punch_fallback"); len(fallbacks) != 0 {
		t.Fatalf("exempt losses recorded punch_fallback: %+v", fallbacks)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}
