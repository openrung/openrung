// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"github.com/openrung/openrung/connectcore/discovery"
)

// Per-user relay access (ADR-006 "Client enrollment and relay authorization").
//
// With an installation key the engine authenticates to the broker (challenge
// → signed auth → access token, cached in memory) and asks the access
// endpoint for relays before connecting. The answer is a signed relay list
// whose client_id values are this installation's short-lived credentials
// (leases), so it flows through the unchanged candidate pipeline: usability,
// targeting, ranking, the ladder and the WSS fallback. While connected the
// supervisor renews the connected relay's lease at about half its lifetime;
// the broker keeps the credential and moves the expiry, so the running tunnel
// never changes. A lease that expires anyway feeds the recovery ladder.
//
// Until relays enforce leases the public list still works, so every failure
// short of an operator disable falls back to it: no key, a broker without the
// access endpoints, an endpoint-unbound front, an error, no relay with
// per-user access for the target, or a broker that will not enroll a new key
// from the current network (which is also surfaced as a notice, since only
// the user can change networks). An empty answer therefore means "no leased
// relay for this target" at connect time (fall back) and "the connected relay
// is gone" at renewal time (reconnect through the ladder); the broker's
// contract for the empty answer is in docs/api.md. How each connect's access
// path went is reported through telemetry (access_outcome.go).

var (
	// ErrAccessDisabled reports that the broker refused this installation
	// because an operator disabled it. It is terminal for the connect.
	ErrAccessDisabled = errors.New("this installation has been disabled and cannot connect; contact OpenRung support")

	// ErrEnrollmentRefused reports that the broker will not enroll this
	// (not yet enrolled) installation from the current network, because it
	// belongs to a datacenter or hosting provider. Retrying from the same
	// network cannot help, so the access path is not retried; an installation
	// enrolled once from another network is never refused this way.
	ErrEnrollmentRefused = errors.New("OpenRung cannot register this device from the current network, which belongs to a hosting or datacenter provider (often another VPN or a cloud server); connect once from your regular internet connection")

	// errAccessUnsupported marks a broker without the user/access endpoints.
	errAccessUnsupported = errors.New("broker does not issue per-user relay access")
	// errLeaseExpired and errLeaseReplaced end a session whose credential the
	// relay no longer accepts (or soon will not); recovery fetches a new one.
	errLeaseExpired  = errors.New("relay access expired")
	errLeaseReplaced = errors.New("relay access was replaced by the broker")
	// errLeaseRelayGone ends a session whose relay the broker no longer
	// lists: a renewal answered with no relays. The relay is most likely
	// gone, so recovery re-ladders at once instead of retrying it.
	errLeaseRelayGone = errors.New("the connected relay is no longer listed by the broker")
)

const (
	// accessTokenRefreshMargin re-authenticates before the token expires, so
	// a renewal never presents a token the broker is about to refuse.
	accessTokenRefreshMargin = 5 * time.Minute
	// accessUnappliedRetries bounds the 503 "issued but not installed yet"
	// retries of one access request; accessUnappliedMaxWait caps each wait.
	accessUnappliedRetries = 2
	accessUnappliedMaxWait = 5 * time.Second
	accessUnappliedDefault = 2 * time.Second
	// leaseRenewalPoll caps how long the renewer sleeps between wall-clock
	// checks, so a host that slept past the renewal time (timers do not run
	// during system sleep) renews or recovers soon after waking.
	leaseRenewalPoll = 30 * time.Second
	// leaseRenewalMaxBackoff caps the wait between failed renewals.
	leaseRenewalMaxBackoff = time.Minute
)

// accessLease describes the credentials of one access fetch: when they stop
// working (on the local clock, derived from the broker's server_time so
// clock skew cannot shorten or stretch it), how long they were issued for,
// and the front that issued them (where renewals go).
type accessLease struct {
	expiresAt time.Time
	lifetime  time.Duration
	front     string
	azureSNI  bool
}

func (l accessLease) valid() bool { return !l.expiresAt.IsZero() && l.lifetime > 0 }

// installationKey resolves the key hook: the desktop/CLI field, else the
// mobile host's.
func (s *Engine) installationKey() InstallationKey {
	if s.Key != nil {
		return s.Key
	}
	if s.Mobile != nil {
		return s.Mobile.Key
	}
	return nil
}

func (s *Engine) leaseNow() time.Time {
	if s.leaseClock != nil {
		return s.leaseClock()
	}
	// Strip the monotonic reading: lease expiry is a wall-clock fact, and a
	// host that slept must compare against the time that actually passed.
	return time.Now().Round(0)
}

func (s *Engine) leaseAfter(d time.Duration) <-chan time.Time {
	if s.leaseTimer != nil {
		return s.leaseTimer(d)
	}
	return time.After(d)
}

func (s *Engine) accessClient() *brokerapi.Client {
	return brokerapi.NewClient(s.brokerHTTPClient(), brokerapi.Options{
		AppVersion:      s.appVersion(),
		PlatformVersion: s.platformVersion(),
		Platform:        s.telemetryPlatform(),
	})
}

// accessWanted reports whether this connect should try per-user access. A
// label target cannot be expressed to the broker (labels are not unique), so
// it keeps the public list until enforcement.
func (s *Engine) accessWanted(conn *connection, target RelayTarget) bool {
	if s.installationKey() == nil || conn.accessUnsupported {
		return false
	}
	return strings.TrimSpace(target.RelayID) != "" || strings.TrimSpace(target.Label) == ""
}

// fetchAccess asks front for per-user access to the target. ok=false (with a
// nil error) means "use the public list"; a non-nil error is terminal for the
// fetch (an operator disable, or cancellation).
func (s *Engine) fetchAccess(ctx context.Context, conn *connection, front string, azureSNI bool, target RelayTarget) (discovery.Fetch, bool, error) {
	if brokerapi.EndpointUnboundBrokerFront(front) && !azureSNI {
		conn.accessOutcome = accessOutcomeFrontUnbound
		s.appendLog("this broker front cannot carry per-user relay access; using the public relay list")
		return discovery.Fetch{}, false, nil
	}
	request := brokerapi.AccessRequest{Limit: RelayLimit}
	if id := strings.TrimSpace(target.RelayID); id != "" {
		request.RelayID, request.Limit = id, 1
	} else if cc := strings.TrimSpace(target.Country); cc != "" {
		request.Country = strings.ToUpper(cc)
	}
	resp, received, err := s.requestAccess(ctx, front, azureSNI, request)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return discovery.Fetch{}, false, ctx.Err()
	case errors.Is(err, ErrAccessDisabled):
		conn.accessOutcome = accessOutcomeDenied
		s.reportAccessDisabled()
		return discovery.Fetch{}, false, err
	case errors.Is(err, ErrEnrollmentRefused):
		// Pre-enforcement the public list still carries the connect, like
		// every other non-terminal access failure; the notice tells the
		// user why per-user access is missing. The front answered properly,
		// so it stays pinned.
		conn.accessOutcome = accessOutcomeEnrollmentRefused
		s.reportEnrollmentRefused()
		return discovery.Fetch{}, false, nil
	case errors.Is(err, errAccessUnsupported):
		conn.accessUnsupported = true
		conn.accessOutcome = accessOutcomeUnsupported
		s.appendLog("broker does not issue per-user relay access; using the public relay list")
		return discovery.Fetch{}, false, nil
	default:
		// The next fetch races the fronts again instead of trusting this one.
		conn.accessFront = ""
		conn.accessOutcome = accessFailureOutcome(err)
		s.appendLog(fmt.Sprintf("per-user relay access failed (%v); using the public relay list", err))
		return discovery.Fetch{}, false, nil
	}
	// The answer must survive the same selection the ladder applies —
	// usability (e.g. direct exit) and the target filter — or the connect
	// would fail on it before any fallback could run. An answer with nothing
	// usable is treated like an empty one.
	usable := usableRelays(resp.RelayListResponse)
	if cands, _, err := FilterCandidates(usable, target); err != nil || len(cands) == 0 {
		conn.accessOutcome = accessOutcomeEmpty
		s.appendLog("no usable relay with per-user access matches; using the public relay list")
		return discovery.Fetch{}, false, nil
	}
	lifetime := resp.LeaseExpiresAt.Sub(resp.ServerTime)
	conn.lease = accessLease{expiresAt: received.Add(lifetime), lifetime: lifetime, front: front, azureSNI: azureSNI}
	conn.accessOutcome = accessOutcomeGranted
	s.appendLog(fmt.Sprintf("received per-user access to %d relay(s) for %s", len(resp.Relays), lifetime.Round(time.Second)))
	return discovery.Fetch{BrokerURL: front, AzureSNI: azureSNI, Response: resp.RelayListResponse}, true, nil
}

// accessFailureOutcome maps a failed access request onto its outcome token:
// the broker's unapplied (503) and rate-limited (429) answers are told apart
// from every other failure (transport, other statuses, authentication).
func accessFailureOutcome(err error) string {
	var status *brokerapi.AccessStatusError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case 503:
			return accessOutcomeUnapplied
		case 429:
			return accessOutcomeRateLimited
		}
	}
	return accessOutcomeError
}

// runPublicFallback walks the public list after every leased candidate
// failed. During the compatibility phase relays without per-user access
// (tunnel-transport, not yet updated) still accept the shared credential, so
// a fleet that mixes both must not fail a connect a public-only relay could
// carry. It applies only when the failed ladder came from per-user access
// and failed remotely; ok=false leaves the caller's error as it was. The
// public list is the one the discovery race already returned, or a fresh
// race when the access request went straight to the pinned front. Relays
// already tried are skipped, failedRelayID (a recovery's dead relay) is
// demoted, and the lease is cleared so no renewal runs for a shared
// credential.
func (s *Engine) runPublicFallback(ctx context.Context, conn *connection, brokerURL string, target RelayTarget, tried []brokerapi.RelayDescriptor, port int, failedRelayID string, ladderErr error) (*candidateResult, ladderOrder, bool, error) {
	if !conn.lease.valid() || ctx.Err() != nil {
		return nil, ladderOrder{}, false, nil
	}
	if _, local := localCandidateErrorStage(ladderErr); local {
		return nil, ladderOrder{}, false, nil
	}
	lease := conn.lease
	conn.lease = accessLease{}
	knownEndpoints := conn.snapshotEndpoints()
	public := conn.publicFallback
	conn.publicFallback = nil
	if public == nil {
		limit := RelayLimit
		if target.Targeted() {
			limit = DirectoryRelayLimit
		}
		fetch, err := s.relayFetcher()(ctx, brokerURL, limit, managerClientID(conn.mgr), s.SessionID())
		if err != nil {
			return nil, ladderOrder{}, false, nil
		}
		_ = conn.mgr.SetBrokerFront(fetch.BrokerURL, fetch.AzureSNI)
		conn.rememberPublicEndpoints(fetch.Response)
		public = &fetch
	}
	// A leased relay this list gave an endpoint the ladder could not try —
	// unknown before, or changed since the cached one it fell back to — gets
	// one attempt there, still on its per-user credential, before the shared
	// credentials take over. Its WSS fronts were already tried.
	if retry := conn.leasedEndpointRetries(tried, knownEndpoints); len(retry) > 0 {
		conn.lease = lease
		retry = demoteRelay(retry, failedRelayID)
		s.appendLog(fmt.Sprintf("retrying %d relay(s) with per-user access at their public endpoint", len(retry)))
		res, err := s.runLadder(ctx, conn, retry, port)
		if err == nil {
			return res, ladderOrder{brokerOrder: retry, ranked: unranked(retry)}, true, nil
		}
		if _, local := localCandidateErrorStage(err); local || ctx.Err() != nil {
			return nil, ladderOrder{}, true, err
		}
		conn.lease = accessLease{}
	}
	cands, _, err := s.candidatesFor(public.Response, target)
	if err != nil {
		return nil, ladderOrder{}, false, nil
	}
	cands = s.adaptToAddressFamily(conn, cands)
	skip := make(map[string]bool, len(tried))
	for _, relay := range tried {
		skip[relay.ID] = true
	}
	remaining := make([]brokerapi.RelayDescriptor, 0, len(cands))
	for _, cand := range cands {
		if !skip[cand.ID] {
			remaining = append(remaining, cand)
		}
	}
	if len(remaining) == 0 {
		return nil, ladderOrder{}, false, nil
	}
	conn.accessOutcome = accessOutcomeFallbackPublic
	s.appendLog(fmt.Sprintf("relays with per-user access failed; trying %d relay(s) from the public list", len(remaining)))
	order := s.rankLadder(ctx, remaining, target)
	ladder := demoteRelay(order.candidates(), failedRelayID)
	s.mu.Lock()
	conn.candidates = ladder
	conn.brokerURL = public.BrokerURL
	s.mu.Unlock()
	res, err := s.runLadder(ctx, conn, ladder, port)
	return res, order, true, err
}

func (s *Engine) reportAccessDisabled() {
	s.appendLog("the broker refused access: this installation has been disabled")
	s.notify(Notice{Kind: NoticeAccessDenied, Reason: ErrAccessDisabled.Error()})
}

func (s *Engine) reportEnrollmentRefused() {
	s.appendLog("the broker does not register new devices from this network (a hosting or datacenter network); using the public relay list")
	s.notify(Notice{Kind: NoticeEnrollmentRefused, Reason: ErrEnrollmentRefused.Error()})
}

// requestAccess runs one access request: it authenticates when no valid
// token is cached, re-authenticates once on 401, and waits out a bounded
// number of 503 "issued but not installed yet" answers (the leases persist,
// so the retry returns the same credentials). received is the local time the
// answer arrived, the anchor for its lease expiry.
func (s *Engine) requestAccess(ctx context.Context, front string, azureSNI bool, request brokerapi.AccessRequest) (brokerapi.AccessResponse, time.Time, error) {
	ctx = brokerapi.WithAzureSNI(ctx, azureSNI)
	client := s.accessClient()
	stale := ""
	unapplied := 0
	for {
		token, err := s.userAccessToken(ctx, client, front, stale)
		if err != nil {
			return brokerapi.AccessResponse{}, time.Time{}, err
		}
		request.AccessToken = token
		resp, err := client.RequestAccess(ctx, front, request)
		if err == nil {
			return resp, s.leaseNow(), nil
		}
		var status *brokerapi.AccessStatusError
		if !errors.As(err, &status) {
			return brokerapi.AccessResponse{}, time.Time{}, err
		}
		switch status.StatusCode {
		case 401:
			if stale != "" {
				return brokerapi.AccessResponse{}, time.Time{}, err
			}
			stale = token
			continue
		case 403:
			return brokerapi.AccessResponse{}, time.Time{}, ErrAccessDisabled
		case 404:
			return brokerapi.AccessResponse{}, time.Time{}, errAccessUnsupported
		case 503:
			if unapplied >= accessUnappliedRetries {
				return brokerapi.AccessResponse{}, time.Time{}, err
			}
			unapplied++
			wait := status.RetryAfter
			if wait <= 0 {
				wait = accessUnappliedDefault
			}
			wait = min(wait, accessUnappliedMaxWait)
			s.appendLog(fmt.Sprintf("relays are still installing access; retrying in %s", wait))
			select {
			case <-ctx.Done():
				return brokerapi.AccessResponse{}, time.Time{}, ctx.Err()
			case <-s.leaseAfter(wait):
			}
		default:
			return brokerapi.AccessResponse{}, time.Time{}, err
		}
	}
}

// accessTokenScopeFor names the broker a front belongs to, for scoping the
// cached bearer token. The compiled-in default fronts are explicitly trusted
// aliases of one broker and share a scope; any other URL (a custom broker, a
// development broker) is its own scope, so a token is never presented to a
// broker other than the one that issued it.
func accessTokenScopeFor(front string) string {
	front = strings.TrimSpace(front)
	for _, alias := range brokerapi.DefaultBrokerURLs() {
		if front == strings.TrimSpace(alias) {
			return "default"
		}
	}
	return "url:" + front
}

// userAccessToken returns the cached access token when it was issued by the
// broker behind front, or authenticates the installation key for a new one
// when none is cached for that broker, it is about to expire, or the cached
// one equals stale (the broker just refused it).
func (s *Engine) userAccessToken(ctx context.Context, client *brokerapi.Client, front, stale string) (string, error) {
	scope := accessTokenScopeFor(front)
	s.accessMu.Lock()
	token, expiry, tokenScope := s.accessToken, s.accessTokenExpiry, s.accessTokenScope
	s.accessMu.Unlock()
	if token != "" && tokenScope == scope && token != stale && s.leaseNow().Add(accessTokenRefreshMargin).Before(expiry) {
		return token, nil
	}

	key := s.installationKey()
	if key == nil {
		return "", errors.New("no installation key")
	}
	spki, err := key.PublicKey()
	if err != nil {
		return "", fmt.Errorf("installation key: %w", err)
	}
	challenge, err := client.RequestUserChallenge(ctx, front)
	if err != nil {
		return "", userAuthError(err)
	}
	request, err := brokerapi.SignUserAuthWith(spki, challenge.Challenge, key.Sign)
	if err != nil {
		return "", fmt.Errorf("installation key: %w", err)
	}
	resp, err := client.AuthenticateUser(ctx, front, request)
	if err != nil {
		return "", userAuthError(err)
	}
	s.accessMu.Lock()
	s.accessToken, s.accessTokenExpiry, s.accessTokenScope = resp.AccessToken, resp.TokenExpiresAt, scope
	s.accessMu.Unlock()
	if store, ok := s.Persistence.(UserIDPersistence); ok {
		_ = store.SaveUserID(resp.UserID)
	}
	return resp.AccessToken, nil
}

// userAuthError maps the user endpoints' refusals onto the access path's
// outcomes: a disabled user is terminal, a refused enrollment from a
// datacenter network is reported, a missing endpoint is "old broker".
func userAuthError(err error) error {
	var status *brokerapi.UserAuthStatusError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case 403:
			if status.Code == brokerapi.UserAuthCodeDatacenterSource {
				return ErrEnrollmentRefused
			}
			return ErrAccessDisabled
		case 404:
			return errAccessUnsupported
		}
	}
	return err
}

// renewLease keeps the connected relay's credential alive: at about half the
// lease lifetime (jittered) it repeats the access request for exactly that
// relay, which extends the lease and returns the same credential. Failures
// retry with backoff until the lease expires. It reports on out — and exits —
// when the lease expired, the broker replaced the credential, or the user
// was disabled; it exits silently when ctx (the candidate's) ends. While the
// engine is paused it holds, and on resume renews at once or reports expiry.
func (s *Engine) renewLease(ctx context.Context, relay brokerapi.RelayDescriptor, lease accessLease, out chan<- error) {
	renewAt := lease.expiresAt.Add(-lease.lifetime).Add(jitter(lease.lifetime / 2))
	failures := 0
	for {
		now := s.leaseNow()
		if !now.Before(lease.expiresAt) {
			s.appendLog(fmt.Sprintf("access to relay %s expired before it could be renewed", relay.ID))
			out <- errLeaseExpired
			return
		}
		if now.Before(renewAt) {
			wait := min(renewAt.Sub(now), leaseRenewalPoll)
			select {
			case <-ctx.Done():
				return
			case <-s.leaseAfter(wait):
			}
			continue
		}
		if !s.awaitResumed(ctx) {
			return
		}
		if !s.leaseNow().Before(lease.expiresAt) {
			continue // slept past expiry while paused: report it above
		}
		resp, received, err := s.requestAccess(ctx, lease.front, lease.azureSNI, brokerapi.AccessRequest{Limit: 1, RelayID: relay.ID})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrAccessDisabled) {
			s.reportAccessDisabled()
			out <- err
			return
		}
		if err == nil && len(resp.Relays) == 1 && resp.Relays[0].ID == relay.ID {
			if resp.Relays[0].ClientID != relay.ClientID {
				// The broker no longer holds this credential (it expired or was
				// revoked there); the running tunnel cannot use a new one.
				s.appendLog(fmt.Sprintf("access to relay %s was replaced; reconnecting", relay.ID))
				out <- errLeaseReplaced
				return
			}
			lifetime := resp.LeaseExpiresAt.Sub(resp.ServerTime)
			lease.expiresAt, lease.lifetime = received.Add(lifetime), lifetime
			renewAt = received.Add(jitter(lifetime / 2))
			failures = 0
			s.appendLog(fmt.Sprintf("renewed access to relay %s for %s", relay.ID, lifetime.Round(time.Second)))
			continue
		}
		if err == nil && len(resp.Relays) == 0 {
			// The broker answered and lists no relay for this ID: the relay
			// left the directory (it stopped heartbeating or was replaced).
			// Its lease cannot be renewed and the relay is probably dead, so
			// recover now rather than retrying it until the lease expires.
			s.appendLog(fmt.Sprintf("relay %s is no longer listed by the broker; reconnecting", relay.ID))
			out <- errLeaseRelayGone
			return
		}
		if err == nil {
			err = errors.New("the broker did not return the connected relay")
		}
		failures++
		delay := min(time.Duration(1<<min(failures, 6))*time.Second, leaseRenewalMaxBackoff)
		renewAt = s.leaseNow().Add(delay)
		s.appendLog(fmt.Sprintf("could not renew access to relay %s (%v); retrying in %s", relay.ID, err, delay))
	}
}
