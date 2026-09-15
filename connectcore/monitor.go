package connectcore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/discovery"
)

const (
	networkRecoveryPollInterval  = 5 * time.Second
	mobileNetworkRecoveryMaxPoll = 60 * time.Second
	mobileNetworkRecoveryLimit   = 2 * time.Minute
)

// supervise owns the connected phase: it watches the live tunnel process, a
// periodic through-tunnel health probe, and the platform network signals, and
// on a trigger runs one automatic recovery pass (fresh relay fetch +
// candidate ladder). It runs in the runConnect goroutine that owns conn and
// never touches s.conn — a user disconnect always wins. Returns ("", nil) on
// a clean end, or the terminal (stage, error) when a recovery pass is
// exhausted. While the engine is paused, a received trigger is held and its
// recovery starts on Resume.
func (s *Engine) supervise(ctx context.Context, conn *connection, cur *candidateResult, port int, target RelayTarget) (string, error) {
	for {
		healthFail := make(chan error, 1)
		healthKick := make(chan struct{}, 1)
		probe := s.healthProber()
		if cur.mobileRun != nil {
			run := cur
			probe = func(ctx context.Context, _ int) error {
				_, err := s.verifyMobilePath(ctx, run, VerificationHealth)
				return err
			}
		}
		if cur.mobileRun != nil {
			done := make(chan struct{})
			cur.healthDone = done
			run := cur
			fronts := s.livenessFronts(conn)
			go func() {
				defer close(done)
				s.healthLoopWithProbe(run.ctx, port, fronts, healthFail, healthKick, probe, run.reporter)
			}()
		} else {
			go s.healthLoopWithProbe(cur.ctx, port, s.livenessFronts(conn), healthFail, healthKick, probe, nil)
		}

		var trigger error
		var triggerReason string
		transportFailure := false
		triggerWasHealth := false
		var recovery *networkRecoveryBudget
	watchTrigger:
		for {
			select {
			case <-ctx.Done():
				return "", nil
			case runErr := <-cur.runDone:
				if ctx.Err() != nil || s.isDisconnecting(conn) {
					return "", nil
				}
				if runErr == nil {
					// The runtime reports nil only for an engine-requested stop,
					// and nobody requested one: treat like any other unexpected
					// exit.
					runErr = errors.New("tunnel exited unexpectedly")
				}
				if cur.accessTransport == accessTransportWSS {
					// The WSS adapter is still alive: this is a local sing-box
					// process failure, not evidence that either the CDN path or the
					// relay failed. A fresh ladder could turn this local crash into a
					// new single-use ticket request, so fail closed and let the user
					// restart after the local fault has been corrected.
					s.appendLog("local tunnel process stopped unexpectedly")
					return "tunnel_process", markLocalCandidateError("active_tunnel_process", runErr)
				}
				trigger = runErr
				triggerReason = "tunnel process exited unexpectedly"
				s.appendLog("tunnel process exited unexpectedly; reconnecting")
				break watchTrigger
			case transportErr := <-cur.transportErr:
				if ctx.Err() != nil || s.isDisconnecting(conn) {
					return "", nil
				}
				if transportErr == nil {
					transportErr = markWSSTransportError("wss_session", cur.frontID, errors.New("WSS access transport stopped"))
				}
				trigger = transportErr
				transportFailure = true
				if gracefulWSSSessionEnd(trigger) {
					triggerReason = "WSS session ended"
					s.appendLog("WSS access transport session ended; reconnecting")
				} else {
					triggerReason = "WSS transport stopped unexpectedly"
					s.appendLog("WSS access transport stopped unexpectedly; reconnecting")
				}
				break watchTrigger
			case probeErr := <-healthFail:
				var held *healthRecoveryError
				if errors.As(probeErr, &held) {
					recovery = held.recovery
				}
				if ctx.Err() != nil || s.isDisconnecting(conn) {
					return "", nil
				}
				if stage, local := localCandidateErrorStage(probeErr); local {
					return stage, probeErr
				}
				if cur.accessTransport == accessTransportWSS {
					// A live WSS socket can still have a blackholed CDN data path.
					// Keep that failure transport-scoped so it never demotes the
					// destination relay or emits relay_attempt_failed.
					trigger = markWSSTransportError("wss_health_probe", cur.frontID, probeErr)
					transportFailure = true
				} else {
					trigger = probeErr
				}
				triggerWasHealth = true
				triggerReason = fmt.Sprintf("tunnel health check failed %d times", HealthFailureThreshold)
				s.appendLog(fmt.Sprintf("tunnel health check failed %d times; reconnecting", HealthFailureThreshold))
				break watchTrigger
			case <-conn.netNotify:
				if ctx.Err() != nil || s.isDisconnecting(conn) {
					return "", nil
				}
				epoch := s.networkEpoch()
				if epoch == conn.handledNetEpoch {
					continue // a wake for an epoch this candidate already covers
				}
				conn.handledNetEpoch = epoch
				if cur.accessTransport == accessTransportWSS {
					// A WSS socket is bound to one physical-network epoch
					// (the mobile invariant): retire the session — orderly,
					// blaming neither the relay nor the front — and recover
					// with a fresh direct-first ladder.
					trigger = markWSSTransportError(wssNetworkEpochStage, cur.frontID, errors.New("physical network epoch changed"))
					transportFailure = true
					triggerReason = "physical network epoch changed"
					s.appendLog("physical network epoch changed; rebuilding the session")
					break watchTrigger
				}
				// A direct or punched path may have survived the change; the
				// end-to-end probe is the authority, so run one now instead of
				// waiting out the probe interval.
				s.appendLog("physical network epoch changed; checking tunnel health now")
				select {
				case healthKick <- struct{}{}:
				default:
				}
			}
		}
		// A trigger received while paused starts its recovery on Resume; a
		// teardown (which cancels ctx) still interrupts the wait.
		if !s.awaitResumed(ctx) {
			return "", nil
		}
		if s.isDisconnecting(conn) {
			return "", nil
		}

		oldRelayID := cur.relay.ID
		failedRelayID := oldRelayID
		if transportFailure {
			// A front/session failure is not evidence against the destination relay.
			failedRelayID = ""
			if gracefulWSSSessionEnd(trigger) {
				s.recordWSSTransportEnded(conn.mgr, oldRelayID, trigger)
			} else {
				s.recordWSSTransportFailed(conn.mgr, oldRelayID, trigger)
			}
		} else {
			// The bare relay_attempt_failed (no attempt measurement — this is not a
			// ladder rung) is what dents the dying relay's broker ranking.
			s.recordRelayAttemptFailed(conn.mgr, oldRelayID, trigger, 0)
		}
		s.notify(Notice{Kind: NoticeFailoverStarted, FromRelayID: oldRelayID, Reason: triggerReason})
		// Keep the last relay label during recovery: the user sees connecting
		// plus log lines, not a bogus disconnect.
		s.setStatus(StatusConnecting, keepLabel, clearError)
		cur.teardown()
		s.mu.Lock()
		conn.active = nil
		s.mu.Unlock()
		// Let traffic fall back to the normal network during the reconnect gap
		// instead of blackholing it against the dead loopback port.
		s.releaseProxy(conn)
		// One budget for this entire recovery, including punch classification,
		// outage waits, and retries. A failed ladder after expiry is terminal
		// for every mobile transport. Carry a preceding health hold forward.
		if recovery == nil {
			recovery = s.newRecoveryBudget()
		}

		// A punched path's loss feeds the per-relay recovery circuit breaker
		// (PunchRecoveryCircuitBreaker.kt / .swift via punchbreaker.go). The
		// counting rule is Android's: a health-probe failure always counts
		// (its gate allowed recovery), an unsolicited
		// tunnel death counts only when the physical network is up — the
		// network-alive gate is the engine's active probe, the analog of
		// OpenRungVpnService.physicalNetworkAlive(). An exempt loss waits for
		// the network first (Android awaits it only when it was down); a
		// counted one waits the breaker's jittered backoff before the
		// re-ladder, and an opened circuit records punch_fallback and makes
		// every later punch attempt for this relay skip to the hub.
		if cur.accessTransport == "punch" {
			// Desktop proxy health already proved front reachability. Desktop
			// TUN skips that gate, while mobile's bounded health hold can expire
			// without proving liveness. Recheck after TUN teardown in both cases:
			// a physical outage is an exempt loss, not punch instability.
			counted := (triggerWasHealth && !s.tunMode()) || s.probeNetworkAliveBefore(ctx, s.livenessFronts(conn), recovery)
			decision := conn.punchBreaker.onDirectPathLost(cur.relay.ID, time.Now(), counted)
			if decision.useRelayHub {
				s.appendLog(fmt.Sprintf("punched path to relay %s is unstable; using the relay hub for the rest of this connection", cur.relay.ID))
				if conn.mgr != nil {
					attrs := map[string]string{"failure_reason": "unstable_direct_path"}
					// The engine's telemetry-safe rendering instead of the
					// mobile raw reason.take(256): raw error text can carry
					// local paths, which must never reach the broker.
					if detail := clienttelemetry.ErrorDetail(trigger); detail != "" {
						attrs["failure_detail"] = detail
					}
					conn.mgr.Record("punch_fallback", cur.relay.ID, attrs, map[string]int64{
						"rapid_failure_count": int64(decision.rapidFailures),
						"direct_uptime_ms":    decision.directUptime.Milliseconds(),
						"recovery_delay_ms":   decision.delay.Milliseconds(),
					})
				}
			} else if decision.counted && decision.delay > 0 {
				s.appendLog(fmt.Sprintf("punched path lost %d time(s) in a row; retrying direct in %s", decision.rapidFailures, decision.delay.Round(time.Millisecond)))
			}
			if s.Mobile == nil && !counted && !s.waitForNetworkRecovery(ctx, conn, recovery) {
				return "", nil
			}
			if decision.delay > 0 && !sleepFor(ctx, decision.delay) {
				return "", nil
			}
		}

		var next *candidateResult
		var fetchMS int64
		for {
			if !s.awaitResumed(ctx) {
				return "", nil
			}
			if (s.Mobile != nil || transportFailure) && !s.waitForNetworkRecovery(ctx, conn, recovery) {
				return "", nil
			}
			var err error
			next, fetchMS, _, err = s.reladder(ctx, conn, port, target, failedRelayID)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return "", nil
			}
			if _, local := localCandidateErrorStage(err); s.Mobile != nil && local {
				return "failover_exhausted", err
			}
			// Mobile retries share the original budget across every transport.
			// Once it expires, a failed ladder terminates even if physical probes
			// still say down. Desktop retains its existing WSS retry policy.
			if (s.Mobile != nil || transportFailure) && !s.networkAliveBefore(ctx, s.livenessFronts(conn), recovery) {
				s.appendLog("network went down during recovery; waiting for connectivity")
				continue
			}
			// A recovery that dies after a prior success is a distinct terminal
			// case from a first-connect failure — tag it so the dashboard does
			// not read it as "never connected".
			return "failover_exhausted", err
		}
		if !s.promote(ctx, conn, next, fetchMS, false) {
			return "", nil // user disconnected as the recovery winner came up
		}
		// A recovery is not a second session-level connection success. Record one
		// measured relay_failover instead: the broker credits the winning relay,
		// while attempt/success trends remain one-to-one for this session.
		if conn.mgr != nil {
			attrs := map[string]string{
				"from_relay_id": oldRelayID,
				"transport":     next.accessTransport,
			}
			if next.frontID != "" {
				attrs["front_id"] = next.frontID
			}
			if reason := clienttelemetry.ClassifyError(trigger); reason != "" {
				attrs["failure_reason"] = reason
			}
			conn.mgr.Record("relay_failover", next.relay.ID, attrs, connectMeasurements(next, fetchMS))
			_ = conn.mgr.Flush(ctx)
		}
		s.appendLog(fmt.Sprintf("failed over from relay %s to %s", oldRelayID, next.relay.ID))
		s.notify(Notice{
			Kind:        NoticeFailoverCompleted,
			FromRelayID: oldRelayID,
			RelayID:     next.relay.ID,
			FrontID:     next.frontID,
			Reason:      triggerReason,
		})
		cur = next
	}
}

// reladder is the automatic recovery pass: one fresh relay fetch (honoring a
// 429's Retry-After once, clamped so a hostile front cannot suspend recovery
// indefinitely), the same target filtering — a pinned relay id stays pinned, a
// country target stays in-country — with the relay that just died demoted to
// the end (never excluded: it may be the only relay there is), then the ladder.
// The telemetry session survives: no BeginSession, no terminal events here.
func (s *Engine) reladder(ctx context.Context, conn *connection, port int, target RelayTarget, failedRelayID string) (*candidateResult, int64, string, error) {
	// Rebuild candidates from the configured primary, not the previous winner.
	// A fallback success must not erase a genuine custom override on reconnect.
	brokerURL := conn.discoveryPrimary
	fetch, fetchMS, err := s.fetchCandidates(ctx, conn, brokerURL, target)
	var rateLimited *discovery.RateLimitedError
	if errors.As(err, &rateLimited) {
		wait := rateLimited.RetryAfter
		if wait <= 0 {
			wait = 10 * time.Second
		}
		if wait > MaxRecoveryBackoff {
			wait = MaxRecoveryBackoff
		}
		s.appendLog(fmt.Sprintf("broker rate-limited; retrying in %s", wait))
		select {
		case <-ctx.Done():
			return nil, 0, "", ctx.Err()
		case <-time.After(wait):
		}
		fetch, fetchMS, err = s.fetchCandidates(ctx, conn, brokerURL, target)
	}
	if err != nil {
		return nil, 0, "broker_fetch", err
	}

	cands, stage, err := s.candidatesFor(fetch.Response, target)
	if err != nil {
		return nil, 0, stage, err
	}
	// Rank first, then demote: the relay that just died is demoted for having
	// lost its tunnel, not for being slow, so ranking must not lift it back to
	// the front. Demoting last keeps both invariants — the ladder is in client
	// latency order, and the failed relay is still retried last. (Desktop-only:
	// Android re-ranks by recursing into connect(), which has no demotion.)
	order := s.rankLadder(ctx, cands, target)
	cands = demoteRelay(order.candidates(), failedRelayID)
	s.mu.Lock()
	conn.candidates = cands
	conn.brokerURL = fetch.BrokerURL
	s.mu.Unlock()

	// The stable port was released while fetching and ranking. Recheck it at
	// the last possible moment so a competing process that claimed it during
	// that gap is reported as a local endpoint collision, not as a fleet of
	// failed relays. TUN mode holds no such port.
	if !s.tunMode() {
		if err := EnsureProxyPortAvailable(port); err != nil {
			return nil, 0, "proxy_bind", err
		}
	}
	res, err := s.runLadder(ctx, conn, cands, port)
	if err != nil {
		return nil, 0, "relay_connect", err
	}
	order.annotate(res)
	return res, fetchMS, "", nil
}

// healthLoop probes end-to-end connectivity through the local proxy on a
// jittered interval, under the live candidate's context (it dies with it).
// After HealthFailureThreshold consecutive failures it checks whether the local
// network is alive at all — via protected neutral HTTPS or broker-front TCP
// on mobile, and broker-front TCP on desktop. A mobile down observation skips
// these probes, but a bounded hold still lets the ladder run. Network alive
// means the tunnel itself is dead: report a failover trigger on failCh. Network
// down (a wifi blip, sleep) holds recovery, up to the mobile wait budget.
// A kick runs the next sweep immediately (the supervisor sends one on a
// network epoch a direct path may have survived), and each sweep holds while
// the engine is paused — resuming runs the held sweep right away.
func (s *Engine) healthLoop(ctx context.Context, port int, fronts []string, failCh chan<- error, kick <-chan struct{}) {
	s.healthLoopWithProbe(ctx, port, fronts, failCh, kick, s.healthProber(), nil)
}

func (s *Engine) healthLoopWithProbe(ctx context.Context, port int, fronts []string, failCh chan<- error, kick <-chan struct{}, probe func(context.Context, int) error, reporter *RunTelemetry) {
	base := s.healthTick
	if base <= 0 {
		base = HealthProbeInterval
	}
	nextDelay := func() time.Duration { return jitter(base) }
	cadence := mobileProbeCadence{base: base, allowance: base}
	if reporter != nil {
		nextDelay = func() time.Duration { return base*5/6 + time.Duration(rand.Int63n(int64(base/3)+1)) }
		cadence.sent, cadence.received, cadence.sampled = reporter.traffic()
	}
	pass := nextDelay()
	timer := time.NewTimer(pass)
	defer timer.Stop()
	failures := 0
	var recovery *networkRecoveryBudget
	for {
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-kick:
			forced = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if !s.awaitResumed(ctx) {
			return
		}
		// Reset only after the pause gate: rescheduling before a long pause
		// would let the interval elapse DURING it, making the held sweep and
		// the next one fire back to back on resume — two probe failures in
		// one instant against a three-failure threshold calibrated to demand
		// ~90s of evidence.
		elapsed := pass
		pass = nextDelay()
		timer.Reset(pass)
		if reporter != nil {
			sent, received, present := reporter.traffic()
			if !cadence.due(elapsed, sent, received, present, failures, forced) {
				continue
			}
		}

		err := probe(ctx, port)
		if err == nil {
			if reporter != nil {
				cadence.healthy()
			}
			failures = 0
			recovery = nil
			s.notify(Notice{Kind: NoticeHealthProbe, Threshold: HealthFailureThreshold})
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if _, local := localCandidateErrorStage(err); local {
			select {
			case failCh <- err:
			default:
			}
			return
		}
		if reporter != nil {
			cadence.failed()
		}
		failures++
		// The internal count keeps growing during a prolonged local outage;
		// the notice reads "N of threshold", so cap what hosts are shown.
		notified := min(failures, HealthFailureThreshold)
		if failures < HealthFailureThreshold {
			s.notify(Notice{Kind: NoticeHealthProbe, Failures: notified, Threshold: HealthFailureThreshold})
			continue
		}
		// Mobile protects physical probes outside the tunnel. Desktop TUN
		// cannot do that: its default route would send front dials through the
		// suspect tunnel, disabling failover. Skip the gate for desktop TUN;
		// its recovery pass restores physical routing by tearing down first.
		if recovery == nil {
			recovery = s.newRecoveryBudget()
		}
		if (!s.tunMode() || s.Mobile != nil) && !s.networkAliveBefore(ctx, fronts, recovery) {
			if ctx.Err() != nil {
				return
			}
			s.appendLog("health check failed but the network looks down; assuming a local outage, not failing over")
			s.notify(Notice{
				Kind: NoticeHealthProbe, Failures: notified, Threshold: HealthFailureThreshold,
				Reason: "network looks down; assuming a local outage, not failing over",
			})
			continue
		}
		s.notify(Notice{Kind: NoticeHealthProbe, Failures: notified, Threshold: HealthFailureThreshold})
		failure := fmt.Errorf("tunnel health probe failed %d times: %w", failures, err)
		if s.Mobile != nil {
			// Transfer the health hold's budget to the supervisor: teardown and
			// punch classification must not buy another outage waiting period.
			failure = &healthRecoveryError{cause: failure, recovery: recovery}
		}
		select {
		case failCh <- failure:
		default:
		}
		return
	}
}

// sleepFor waits delay, interruptible by ctx; false when ctx ended first.
func sleepFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// networkAlive accepts either neutral HTTPS or broker-front TCP on mobile.
// Neither set is a prerequisite: either may be regionally blocked. Known-down
// mobile networks skip I/O; desktop retains its broker-front TCP gate.
func (s *Engine) networkAlive(ctx context.Context, fronts []string) bool {
	if ctx.Err() != nil || (s.Mobile != nil && s.physicalNetworkKnownDown()) {
		return false
	}
	if s.checkNetworkAlive != nil {
		return s.checkNetworkAlive(ctx, fronts)
	}
	if s.Mobile != nil && s.physicalNetworkAlive(ctx) {
		return true
	}
	dialer := s.protectedNetDialer(RelayTCPTimeout)
	for _, addr := range fronts {
		if ctx.Err() != nil {
			return false
		}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if isSocketProtectionFailure(err) {
			// The host refuses to protect sockets, so this gate cannot
			// measure the network — and waiting cannot help, since every
			// recovery dial would meet the same refusal. Report alive so the
			// ladder runs and surfaces the terminal LOCAL failure, instead of
			// reading the refusal as "network down" and holding
			// waitForNetworkRecovery (and the user, on CONNECTING) forever.
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

// waitForNetworkRecovery prevents a fatal WSS socket caused by Wi-Fi loss or
// laptop sleep from becoming failover_exhausted. The dead local proxy has
// already been released; recovery starts a fresh direct-first ladder only once
// the physical-network liveness gate succeeds, or the mobile hold expires.
func (s *Engine) waitForNetworkRecovery(ctx context.Context, conn *connection, recovery *networkRecoveryBudget) bool {
	if !s.awaitResumed(ctx) {
		return false
	}
	fronts := s.livenessFronts(conn)
	if s.networkAliveBefore(ctx, fronts, recovery) {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	s.appendLog("the tunnel stopped while the local network is down; waiting for connectivity")
	delay := s.networkRetryDelay
	if delay <= 0 {
		delay = networkRecoveryPollInterval
	}
	baseDelay := delay
	timer := time.NewTimer(recoveryWaitDelay(delay, recovery.currentDeadline(s)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			if s.Mobile != nil {
				delay = nextMobileRecoveryDelay(delay)
			}
		case <-conn.netNotify:
			// A platform network signal rechecks immediately instead of
			// waiting out the poll. Reset backoff for the new physical path.
			delay = baseDelay
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if !s.awaitResumed(ctx) {
			return false
		}
		if s.networkAliveBefore(ctx, fronts, recovery) {
			s.appendLog("physical-network gate released; starting a fresh direct-first ladder")
			return true
		}
		timer.Reset(recoveryWaitDelay(delay, recovery.currentDeadline(s)))
	}
}

// livenessFronts is the broker-front host:port list used as the network-alive
// reference, derived from the same candidates discovery races.
func (s *Engine) livenessFronts(conn *connection) []string {
	cands := brokerapi.BrokerCandidates(s.connBrokerURL(conn))
	seen := make(map[string]struct{}, len(cands.URLs))
	fronts := make([]string, 0, len(cands.URLs))
	for _, raw := range cands.URLs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			continue
		}
		host := parsed.Hostname()
		port := parsed.Port()
		if port == "" {
			port = "443" // discovery endpoints are HTTPS
		}
		addr := net.JoinHostPort(host, port)
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		fronts = append(fronts, addr)
	}
	return fronts
}

func (s *Engine) connBrokerURL(conn *connection) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.brokerURL
}

func (s *Engine) isDisconnecting(conn *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.disconnecting
}

// jitter spreads an interval by ±25% so the through-tunnel health probe is not
// a fixed-period beacon a traffic-analysis classifier could lock onto.
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	delta := time.Duration(rand.Int63n(int64(base)/2+1)) - base/4
	return base + delta
}

// A mobile outage hint must not pin CONNECTED/CONNECTING forever. The budget
// covers a health hold and its complete post-teardown recovery together.
func (s *Engine) mobileRecoveryDeadline() time.Time {
	if s.Mobile == nil {
		return time.Time{}
	}
	budget := s.networkRecoveryLimit
	if budget <= 0 {
		budget = mobileNetworkRecoveryLimit
	}
	return time.Now().Add(budget)
}

// Transfer sole budget ownership from the exiting health worker to supervise.
// Unwrap preserves the remote failure classifier and telemetry error chain.
type healthRecoveryError struct {
	cause    error
	recovery *networkRecoveryBudget
}

func (e *healthRecoveryError) Error() string { return e.cause.Error() }
func (e *healthRecoveryError) Unwrap() error { return e.cause }

// Budgets belong to one recovery, starting at the health hold or teardown, never
// to an individual poll/retry. Resume renews them so suspended time cannot force
// recovery without checking the newly available physical network.
type networkRecoveryBudget struct {
	deadline    time.Time
	resumeEpoch uint64
}

func (s *Engine) newRecoveryBudget() *networkRecoveryBudget {
	return &networkRecoveryBudget{resumeEpoch: s.currentResumeEpoch(), deadline: s.mobileRecoveryDeadline()}
}

func (b *networkRecoveryBudget) currentDeadline(s *Engine) time.Time {
	if epoch := s.currentResumeEpoch(); epoch != b.resumeEpoch {
		b.deadline = s.mobileRecoveryDeadline()
		b.resumeEpoch = epoch
	}
	return b.deadline
}

// probeNetworkAliveBefore measures liveness only. In particular expiry is not
// evidence that a punch loss should count against the relay's circuit breaker.
func (s *Engine) probeNetworkAliveBefore(ctx context.Context, fronts []string, recovery *networkRecoveryBudget) bool {
	if deadline := recovery.currentDeadline(s); !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return s.networkAlive(ctx, fronts)
}

// networkAliveBefore permits the ladder on liveness OR budget exhaustion.
// Cancellation always wins; expiry never permits a further outage retry.
func (s *Engine) networkAliveBefore(ctx context.Context, fronts []string, recovery *networkRecoveryBudget) bool {
	alive := s.probeNetworkAliveBefore(ctx, fronts, recovery)
	if ctx.Err() != nil {
		return false
	}
	if deadline := recovery.currentDeadline(s); !deadline.IsZero() && !time.Now().Before(deadline) {
		s.appendLog("physical-network wait budget exhausted; letting the recovery ladder report its result")
		return true
	}
	return alive
}

func recoveryWaitDelay(delay time.Duration, deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return delay
	}
	return min(delay, max(0, time.Until(deadline)))
}

func nextMobileRecoveryDelay(delay time.Duration) time.Duration {
	return min(2*delay, mobileNetworkRecoveryMaxPoll)
}
