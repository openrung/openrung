package vpnservice

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/desktop/proxyconfig"
	"openrung/internal/clienttelemetry"
)

const networkRecoveryPollInterval = 5 * time.Second

// supervise owns the connected phase: it watches the live tunnel process and a
// periodic through-tunnel health probe, and on either trigger runs one
// automatic recovery pass (fresh relay fetch + candidate ladder). It runs in
// the runConnect goroutine that owns conn and never touches s.conn — a user
// disconnect always wins. Returns ("", nil) on a clean end, or the terminal
// (stage, error) when a recovery pass is exhausted.
func (s *Service) supervise(ctx context.Context, conn *connection, cur *candidateResult, port int, targetCountry, targetRelayID string) (string, error) {
	for {
		healthFail := make(chan error, 1)
		go s.healthLoop(cur.ctx, port, s.livenessFronts(conn), healthFail)

		var trigger error
		transportFailure := false
		select {
		case <-ctx.Done():
			return "", nil
		case runErr := <-cur.runErrCh:
			cur.reaped = true
			if ctx.Err() != nil || s.isDisconnecting(conn) {
				return "", nil
			}
			if runErr == nil {
				// Run returns nil only on the cancel path, and nobody cancelled:
				// treat like any other unexpected exit.
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
			s.appendLog("tunnel process exited unexpectedly; reconnecting")
		case transportErr := <-cur.transportErr:
			if ctx.Err() != nil || s.isDisconnecting(conn) {
				return "", nil
			}
			if transportErr == nil {
				transportErr = markWSSTransportError("wss_session", cur.frontID, errors.New("WSS access transport stopped"))
			}
			trigger = transportErr
			transportFailure = true
			s.appendLog("WSS access transport stopped unexpectedly; reconnecting")
		case probeErr := <-healthFail:
			if ctx.Err() != nil || s.isDisconnecting(conn) {
				return "", nil
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
			s.appendLog(fmt.Sprintf("tunnel health check failed %d times; reconnecting", config.HealthFailureThreshold))
		}

		oldRelayID := cur.relay.ID
		failedRelayID := oldRelayID
		if transportFailure {
			// A front/session failure is not evidence against the destination relay.
			failedRelayID = ""
			s.recordWSSTransportFailed(conn.mgr, oldRelayID, trigger)
		} else {
			// The bare relay_attempt_failed (no attempt measurement — this is not a
			// ladder rung) is what dents the dying relay's broker ranking.
			s.recordRelayAttemptFailed(conn.mgr, oldRelayID, trigger, 0)
		}
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

		var next *candidateResult
		var fetchMS int64
		for {
			if transportFailure && !s.waitForNetworkRecovery(ctx, conn) {
				return "", nil
			}
			var err error
			next, fetchMS, _, err = s.reladder(ctx, conn, port, targetCountry, targetRelayID, failedRelayID)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return "", nil
			}
			// If connectivity vanished during a WSS-triggered recovery, return to
			// the local-outage gate and run another fresh direct-first ladder later.
			if transportFailure && !s.networkAlive(ctx, s.livenessFronts(conn)) {
				s.appendLog("network went down during WSS recovery; waiting for connectivity")
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
		cur = next
	}
}

// reladder is the automatic recovery pass: one fresh relay fetch (honoring a
// 429's Retry-After once, clamped so a hostile front cannot suspend recovery
// indefinitely), the same target filtering — a pinned relay id stays pinned, a
// country target stays in-country — with the relay that just died demoted to
// the end (never excluded: it may be the only relay there is), then the ladder.
// The telemetry session survives: no BeginSession, no terminal events here.
func (s *Service) reladder(ctx context.Context, conn *connection, port int, targetCountry, targetRelayID, failedRelayID string) (*candidateResult, int64, string, error) {
	brokerURL := s.connBrokerURL(conn)
	fetch, fetchMS, err := s.fetchCandidates(ctx, conn, brokerURL, targetCountry, targetRelayID)
	var rateLimited *discovery.RateLimitedError
	if errors.As(err, &rateLimited) {
		wait := rateLimited.RetryAfter
		if wait <= 0 {
			wait = 10 * time.Second
		}
		if wait > config.MaxRecoveryBackoff {
			wait = config.MaxRecoveryBackoff
		}
		s.appendLog(fmt.Sprintf("broker rate-limited; retrying in %s", wait))
		select {
		case <-ctx.Done():
			return nil, 0, "", ctx.Err()
		case <-time.After(wait):
		}
		fetch, fetchMS, err = s.fetchCandidates(ctx, conn, brokerURL, targetCountry, targetRelayID)
	}
	if err != nil {
		return nil, 0, "broker_fetch", err
	}

	cands, stage, err := s.candidatesFor(fetch.Response, targetCountry, targetRelayID)
	if err != nil {
		return nil, 0, stage, err
	}
	// Rank first, then demote: the relay that just died is demoted for having
	// lost its tunnel, not for being slow, so ranking must not lift it back to
	// the front. Demoting last keeps both invariants — the ladder is in client
	// latency order, and the failed relay is still retried last. (Desktop-only:
	// Android re-ranks by recursing into connect(), which has no demotion.)
	order := s.rankLadder(ctx, cands, targetRelayID)
	cands = demoteRelay(order.candidates(), failedRelayID)
	s.mu.Lock()
	conn.candidates = cands
	conn.brokerURL = fetch.BrokerURL
	s.mu.Unlock()

	// The stable port was released while fetching and ranking. Recheck it at
	// the last possible moment so a competing process that claimed it during
	// that gap is reported as a local endpoint collision, not as a fleet of
	// failed relays.
	if err := proxyconfig.EnsureAvailable(port); err != nil {
		return nil, 0, "proxy_bind", err
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
// network is alive at all — by dialing the broker fronts, which are far more
// available than any single relay and independent of the tunnel. Network alive
// means the tunnel itself is dead: report a failover trigger on failCh. Network
// down (a wifi blip, sleep) means leave the tunnel alone and keep probing.
func (s *Service) healthLoop(ctx context.Context, port int, fronts []string, failCh chan<- error) {
	base := s.healthTick
	if base <= 0 {
		base = config.HealthProbeInterval
	}
	timer := time.NewTimer(jitter(base))
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(jitter(base))

		err := s.healthProber()(ctx, port)
		if err == nil {
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		if failures < config.HealthFailureThreshold {
			continue
		}
		if !s.networkAlive(ctx, fronts) {
			if ctx.Err() != nil {
				return
			}
			s.appendLog("health check failed but the network looks down; assuming a local outage, not failing over")
			continue
		}
		select {
		case failCh <- fmt.Errorf("tunnel health probe failed %d times: %w", failures, err):
		default:
		}
		return
	}
}

// networkAlive dials the broker fronts to decide whether the local network is
// up. The fronts (Cloudflare + CloudFront, plus any user override) are highly
// available and independent of the relay fleet, so unlike dialing the candidate
// relays this never mistakes a single dead relay for a local outage.
func (s *Service) networkAlive(ctx context.Context, fronts []string) bool {
	if s.checkNetworkAlive != nil {
		return s.checkNetworkAlive(ctx, fronts)
	}
	for _, addr := range fronts {
		dialer := net.Dialer{Timeout: config.RelayTCPTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
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
// an independent HTTPS broker front is reachable again.
func (s *Service) waitForNetworkRecovery(ctx context.Context, conn *connection) bool {
	fronts := s.livenessFronts(conn)
	if s.networkAlive(ctx, fronts) {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	s.appendLog("WSS transport stopped while the local network is down; waiting for connectivity")
	delay := s.networkRetryDelay
	if delay <= 0 {
		delay = networkRecoveryPollInterval
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			if s.networkAlive(ctx, fronts) {
				s.appendLog("network connectivity restored; starting a fresh direct-first ladder")
				return true
			}
			timer.Reset(delay)
		}
	}
}

// livenessFronts is the broker-front host:port list used as the network-alive
// reference, derived from the same candidates discovery races.
func (s *Service) livenessFronts(conn *connection) []string {
	cands := config.BrokerCandidates(s.connBrokerURL(conn))
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

func (s *Service) connBrokerURL(conn *connection) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.brokerURL
}

func (s *Service) isDisconnecting(conn *connection) bool {
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
