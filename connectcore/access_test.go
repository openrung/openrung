// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/discovery"
)

// fakeClock drives lease renewal and expiry: Now is settable and After
// channels fire only when Advance passes their deadline.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now().UTC().Truncate(time.Second)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// fakeAccessLease is the fake broker's record of one (user, relay) lease.
type fakeAccessLease struct {
	credential string
	expiresAt  time.Time
}

// fakeAccessBroker implements the user and access endpoints (plus a sink for
// telemetry) on loopback, where brokerapi accepts unsigned bodies. Leases
// follow the broker contract: a live lease is extended keeping its
// credential, an expired one is replaced by a new credential.
type fakeAccessBroker struct {
	t      *testing.T
	srv    *httptest.Server
	clock  func() time.Time
	ttl    time.Duration
	relays []brokerapi.RelayDescriptor

	mu         sync.Mutex
	leases     map[string]fakeAccessLease
	issued     int
	requests   []brokerapi.AccessRequest
	challenges atomic.Int64
	auths      atomic.Int64
	// Behaviour switches.
	noEndpoints  bool                             // 404 on every user/access route (an older broker)
	disabled     bool                             // 403 on auth and access
	datacenter   bool                             // 403 datacenter_source on auth (a new key from a datacenter network)
	unapplied    int                              // answer this many access requests with 503 first
	failAccess   bool                             // 500 on access
	failRenewals bool                             // 500 on single-relay requests (the renewal shape)
	emptyAccess  bool                             // 200 with no relays
	mutateAccess func(*brokerapi.RelayDescriptor) // applied to every relay the access route returns
	rejectTokens int                              // answer this many access requests with 401 first
	tokenPrefix  string
	bearers      []string                   // every Authorization header the access route saw
	events       []brokerapi.TelemetryEvent // every telemetry event uploaded to this broker
}

func newFakeAccessBroker(t *testing.T, clock func() time.Time, relays ...brokerapi.RelayDescriptor) *fakeAccessBroker {
	t.Helper()
	b := &fakeAccessBroker{t: t, clock: clock, ttl: time.Hour, relays: relays, leases: map[string]fakeAccessLease{}, tokenPrefix: "token-"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/challenge", func(w http.ResponseWriter, r *http.Request) {
		if b.noEndpoints {
			http.NotFound(w, r)
			return
		}
		n := b.challenges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.UserChallenge{Challenge: fmt.Sprintf("challenge-%d", n), ExpiresAt: time.Now().Add(time.Minute)})
	})
	mux.HandleFunc("POST /api/v1/users/auth", func(w http.ResponseWriter, r *http.Request) {
		if b.noEndpoints {
			http.NotFound(w, r)
			return
		}
		var req brokerapi.UserAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		pub, err := brokerapi.ParseUserPublicKey(req.PublicKey)
		if err != nil || !brokerapi.VerifyUserAuth(pub, req.Challenge, req.Signature) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-OpenRung-Client-ID") != "" {
			t.Error("user auth carried the telemetry client ID")
		}
		n := b.auths.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if b.disabled {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(brokerapi.UserAuthResponse{UserID: "user_test", Status: brokerapi.UserStatusDisabled})
			return
		}
		if b.datacenter {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{Error: "enrollment is not available from this network", Code: brokerapi.UserAuthCodeDatacenterSource})
			return
		}
		_ = json.NewEncoder(w).Encode(brokerapi.UserAuthResponse{
			UserID: "user_test", Status: brokerapi.UserStatusEnabled,
			AccessToken: fmt.Sprintf("%s%d", b.tokenPrefix, n), TokenExpiresAt: time.Now().Add(24 * time.Hour),
		})
	})
	mux.HandleFunc("POST /api/v1/access", b.access)
	mux.HandleFunc("POST /api/v1/telemetry/events", func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Events []brokerapi.TelemetryEvent `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.events = append(b.events, batch.Events...)
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeAccessBroker) access(w http.ResponseWriter, r *http.Request) {
	if b.noEndpoints {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-OpenRung-Client-ID") != "" || r.Header.Get("X-OpenRung-Session-ID") != "" {
		b.t.Error("access request carried the telemetry identity")
	}
	var req brokerapi.AccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, req)
	b.bearers = append(b.bearers, r.Header.Get("Authorization"))
	switch {
	case !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+b.tokenPrefix):
		w.WriteHeader(http.StatusUnauthorized)
		return
	case b.rejectTokens > 0:
		b.rejectTokens--
		w.WriteHeader(http.StatusUnauthorized)
		return
	case b.disabled:
		w.WriteHeader(http.StatusForbidden)
		return
	case b.failAccess, b.failRenewals && req.RelayID != "" && req.Limit == 1:
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	now := b.clock()
	var picked []brokerapi.RelayDescriptor
	if !b.emptyAccess {
		for _, relay := range b.relays {
			if req.RelayID != "" && relay.ID != req.RelayID {
				continue
			}
			if req.Country != "" && !strings.EqualFold(relay.CountryCode, req.Country) {
				continue
			}
			lease, ok := b.leases[relay.ID]
			if !ok || !lease.expiresAt.After(now) {
				b.issued++
				lease.credential = fmt.Sprintf("lease-%s-%d", relay.ID, b.issued)
			}
			lease.expiresAt = now.Add(b.ttl)
			b.leases[relay.ID] = lease
			relay.ClientID = lease.credential
			// The directory lease of a live relay is always current on the
			// broker's clock.
			relay.ExpiresAt = now.Add(3 * time.Minute)
			if b.mutateAccess != nil {
				b.mutateAccess(&relay)
			}
			picked = append(picked, relay)
			if req.Limit > 0 && len(picked) == req.Limit {
				break
			}
		}
	}
	if b.unapplied > 0 && len(picked) > 0 {
		b.unapplied--
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	resp := brokerapi.AccessResponse{RelayListResponse: brokerapi.RelayListResponse{
		Count: len(picked), ServerTime: now, NotAfter: now.Add(30 * time.Minute), Channel: brokerapi.ChannelAccess, Relays: picked,
	}}
	if resp.Relays == nil {
		resp.Relays = []brokerapi.RelayDescriptor{}
	}
	if len(picked) > 0 {
		resp.LeaseExpiresAt = now.Add(b.ttl)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		b.t.Fatal(err)
	}
	// The relay wire schema requires the deprecated volunteer_version key.
	body = []byte(strings.ReplaceAll(string(body), `"relay_version":`, `"volunteer_version":"1","relay_version":`))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (b *fakeAccessBroker) accessRequests() []brokerapi.AccessRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]brokerapi.AccessRequest(nil), b.requests...)
}

func (b *fakeAccessBroker) lease(relayID string) fakeAccessLease {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.leases[relayID]
}

func (b *fakeAccessBroker) set(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn()
}

// event returns the first uploaded telemetry event with that name.
func (b *fakeAccessBroker) event(name string) (brokerapi.TelemetryEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.events {
		if e.Event == name {
			return e, true
		}
	}
	return brokerapi.TelemetryEvent{}, false
}

// awaitEvent waits for the engine to upload the named event (connect events
// are flushed right after they are recorded) and checks the access-path
// attributes it carries; "" for relaySource means the key must be absent.
func awaitEvent(t *testing.T, broker *fakeAccessBroker, name, accessOutcome, relaySource string) brokerapi.TelemetryEvent {
	t.Helper()
	var event brokerapi.TelemetryEvent
	waitUntil(t, "the "+name+" event", func() bool {
		var ok bool
		event, ok = broker.event(name)
		return ok
	})
	if got := event.Attributes["access_outcome"]; got != accessOutcome {
		t.Fatalf("%s access_outcome = %q, want %q (attributes %v)", name, got, accessOutcome, event.Attributes)
	}
	if got := event.Attributes["relay_source"]; got != relaySource {
		t.Fatalf("%s relay_source = %q, want %q (attributes %v)", name, got, relaySource, event.Attributes)
	}
	return event
}

// accessEngine is a ladder engine with an installation key whose public
// list fetches report the fake broker as the winning front. It records the
// credential every tunnel run was configured with.
type accessEngine struct {
	*Engine
	sink   *testSink
	key    *memoryKey
	mu     sync.Mutex
	runs   []string // the client_id of each started tunnel
	public atomic.Int64
}

func newAccessEngine(t *testing.T, broker *fakeAccessBroker, publicRelays ...brokerapi.RelayDescriptor) *accessEngine {
	t.Helper()
	s, sink := newLadderService(t, func() []brokerapi.RelayDescriptor { return publicRelays })
	e := &accessEngine{Engine: s, sink: sink, key: newMemoryKey(t)}
	s.Key = e.key
	s.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		e.public.Add(1)
		return discovery.Fetch{BrokerURL: broker.srv.URL, Response: listOf(publicRelays...)}, nil
	}
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, configJSON []byte) error {
		var config struct {
			Outbounds []struct {
				UUID string `json:"uuid"`
			} `json:"outbounds"`
		}
		_ = json.Unmarshal(configJSON, &config)
		uuid := ""
		for _, outbound := range config.Outbounds {
			if outbound.UUID != "" {
				uuid = outbound.UUID
			}
		}
		e.mu.Lock()
		e.runs = append(e.runs, uuid)
		e.mu.Unlock()
		<-ctx.Done()
		return nil
	})
	return e
}

func (e *accessEngine) tunnelRuns() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.runs...)
}

func (e *accessEngine) useClock(clock *fakeClock) {
	e.leaseClock = clock.Now
	e.leaseTimer = clock.After
}

func publicRelay(id, country string) brokerapi.RelayDescriptor {
	r := usableRelay(id, country, "City", "Country")
	r.ClientID = "shared-" + id
	return r
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAccessConnectUsesLeaseCredentialFromTheRaceWinner(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP"), publicRelay("relay-b", "DE")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	persist := &userIDRecorder{}
	e.Persistence = persist

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	runs := e.tunnelRuns()
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "lease-relay-") {
		t.Fatalf("tunnel credentials = %v, want one per-user lease", runs)
	}
	requests := broker.accessRequests()
	if len(requests) != 1 || requests[0].Limit != RelayLimit || requests[0].RelayID != "" || requests[0].Country != "" {
		t.Fatalf("access requests = %+v", requests)
	}
	if e.public.Load() != 1 {
		t.Fatalf("public list fetched %d times, want the one front race", e.public.Load())
	}
	if persist.get() != "user_test" {
		t.Fatalf("persisted user ID = %q", persist.get())
	}
	awaitEvent(t, broker, "connection_succeeded", accessOutcomeGranted, relaySourceAccess)
	if err := e.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e.Engine)

	// A second connect reuses the cached token: no new challenge.
	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	if broker.challenges.Load() != 1 || broker.auths.Load() != 1 {
		t.Fatalf("challenges=%d auths=%d, want the token reused", broker.challenges.Load(), broker.auths.Load())
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

type userIDRecorder struct {
	fakePersistence
	idMu sync.Mutex
	id   string
}

func (r *userIDRecorder) SaveUserID(id string) error {
	r.idMu.Lock()
	defer r.idMu.Unlock()
	r.id = id
	return nil
}

func (r *userIDRecorder) get() string {
	r.idMu.Lock()
	defer r.idMu.Unlock()
	return r.id
}

func TestAccessTargetedConnectRequestsOnlyTheTarget(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP"), publicRelay("relay-b", "DE")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	e := newAccessEngine(t, broker, relays...)

	if err := e.Connect("https://configured.example", "", "relay-b"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	requests := broker.accessRequests()
	if len(requests) != 1 || requests[0].RelayID != "relay-b" || requests[0].Limit != 1 || requests[0].Country != "" {
		t.Fatalf("access requests = %+v", requests)
	}
	if broker.lease("relay-a").credential != "" {
		t.Fatal("a relay-targeted connect leased another relay")
	}
	if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != broker.lease("relay-b").credential {
		t.Fatalf("tunnel credentials = %v", runs)
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)

	if err := e.Connect("https://configured.example", "de", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	requests = broker.accessRequests()
	if last := requests[len(requests)-1]; last.Country != "DE" || last.RelayID != "" || last.Limit != RelayLimit {
		t.Fatalf("country access request = %+v", last)
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

func TestAccessRetriesUnappliedAndExpiredToken(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	broker.unapplied = 1
	broker.rejectTokens = 1
	e := newAccessEngine(t, broker, relays...)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	runs := e.tunnelRuns()
	if len(runs) != 1 || runs[0] != broker.lease("relay-a").credential {
		t.Fatalf("tunnel credentials = %v, want the lease issued before the 503", runs)
	}
	// 401 → one re-authentication; 503 → one retry.
	if broker.auths.Load() != 2 || len(broker.accessRequests()) != 3 {
		t.Fatalf("auths=%d access requests=%d", broker.auths.Load(), len(broker.accessRequests()))
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

func TestAccessFallsBackToThePublicList(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	for name, tc := range map[string]struct {
		setup   func(*fakeAccessBroker, *accessEngine)
		outcome string
	}{
		"no key":            {func(_ *fakeAccessBroker, e *accessEngine) { e.Key = nil }, accessOutcomeNoKey},
		"older broker":      {func(b *fakeAccessBroker, _ *accessEngine) { b.noEndpoints = true }, accessOutcomeUnsupported},
		"no access relays":  {func(b *fakeAccessBroker, _ *accessEngine) { b.emptyAccess = true }, accessOutcomeEmpty},
		"access error":      {func(b *fakeAccessBroker, _ *accessEngine) { b.failAccess = true }, accessOutcomeError},
		"label target only": {func(*fakeAccessBroker, *accessEngine) {}, accessOutcomeNotRequested},
	} {
		t.Run(name, func(t *testing.T) {
			broker := newFakeAccessBroker(t, time.Now, relays...)
			e := newAccessEngine(t, broker, relays...)
			tc.setup(broker, e)
			target := RelayTarget{}
			if name == "label target only" {
				target.Label = relays[0].Label
				e.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
					labelled := relays[0]
					labelled.Label = "home"
					return discovery.Fetch{BrokerURL: broker.srv.URL, Response: listOf(labelled)}, nil
				}
				target.Label = "home"
			}
			if err := e.ConnectTarget("https://configured.example", target); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, e.Engine, StatusConnected)
			if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != "shared-relay-a" {
				t.Fatalf("tunnel credentials = %v, want the public credential", runs)
			}
			// The fallback is visible to the broker: the outcome names why
			// the public list was used, and the relay came from it.
			awaitEvent(t, broker, "connection_succeeded", tc.outcome, relaySourcePublic)
			_ = e.Disconnect()
			waitIdle(t, e.Engine)
			if name == "no key" || name == "label target only" {
				if broker.challenges.Load() != 0 || len(broker.accessRequests()) != 0 {
					t.Fatal("access was attempted")
				}
			}
		})
	}
}

func TestAccessDisabledUserIsTerminal(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	broker.disabled = true
	e := newAccessEngine(t, broker, relays...)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	state := waitForStatus(t, e.Engine, StatusFailed)
	waitIdle(t, e.Engine)
	if state.LastError == nil || !strings.Contains(*state.LastError, "disabled") {
		t.Fatalf("lastError = %v", state.LastError)
	}
	if runs := e.tunnelRuns(); len(runs) != 0 {
		t.Fatalf("a disabled installation started tunnels: %v", runs)
	}
	if !hasNotice(e.sink, NoticeAccessDenied) {
		t.Fatal("no access_denied notice")
	}
	failed := awaitEvent(t, broker, "connection_failed", accessOutcomeDenied, "")
	if failed.Attributes["failure_stage"] != "access_denied" {
		t.Fatalf("failure_stage = %q", failed.Attributes["failure_stage"])
	}
}

// A broker that will not enroll the key from the current (datacenter)
// network is not a disable: the connect continues on the public list
// (pre-enforcement), the user gets a notice saying why, the outcome token
// names the refusal, and the refused authentication is not retried.
func TestAccessEnrollmentRefusedFallsBackWithNotice(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	broker.datacenter = true
	e := newAccessEngine(t, broker, relays...)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != "shared-relay-a" {
		t.Fatalf("tunnel credentials = %v, want the public credential", runs)
	}
	if hasNotice(e.sink, NoticeAccessDenied) {
		t.Fatal("a refused enrollment was reported as a disabled installation")
	}
	var refused *Notice
	for _, notice := range e.sink.noticesSnapshot() {
		if notice.Kind == NoticeEnrollmentRefused {
			refused = &notice
		}
	}
	if refused == nil || refused.Reason != ErrEnrollmentRefused.Error() {
		t.Fatalf("enrollment_refused notice = %+v", refused)
	}
	if got := broker.auths.Load(); got != 1 {
		t.Fatalf("user authentications = %d, want the one refused attempt", got)
	}
	if len(broker.accessRequests()) != 0 {
		t.Fatal("an access request was sent without a token")
	}
	awaitEvent(t, broker, "connection_succeeded", accessOutcomeEnrollmentRefused, relaySourcePublic)
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

func TestUserAuthErrorTellsRefusalsApart(t *testing.T) {
	for _, tc := range []struct {
		status *brokerapi.UserAuthStatusError
		want   error
	}{
		{&brokerapi.UserAuthStatusError{StatusCode: 403}, ErrAccessDisabled},
		{&brokerapi.UserAuthStatusError{StatusCode: 403, Code: brokerapi.UserAuthCodeDatacenterSource}, ErrEnrollmentRefused},
		{&brokerapi.UserAuthStatusError{StatusCode: 403, Code: "something_else"}, ErrAccessDisabled},
		{&brokerapi.UserAuthStatusError{StatusCode: 404}, errAccessUnsupported},
	} {
		if got := userAuthError(tc.status); !errors.Is(got, tc.want) {
			t.Errorf("%+v → %v, want %v", tc.status, got, tc.want)
		}
	}
	unclassified := &brokerapi.UserAuthStatusError{StatusCode: 503, Code: brokerapi.UserAuthCodeSourceUnclassified}
	if got := userAuthError(unclassified); got != unclassified {
		t.Errorf("503 source_unclassified → %v, want the status error itself (an ordinary access error)", got)
	}
}

// Exhausting the bounded 503 retries falls back to the public list and
// reports it as unapplied, distinct from any other access error.
func TestAccessUnappliedBeyondRetriesFallsBack(t *testing.T) {
	clock := newFakeClock()
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, clock.Now, relays...)
	broker.unapplied = accessUnappliedRetries + 1
	e := newAccessEngine(t, broker, relays...)
	e.useClock(clock)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < accessUnappliedRetries; i++ {
		waitUntil(t, "the unapplied retry wait", func() bool { return clock.pending() > 0 })
		clock.Advance(accessUnappliedMaxWait)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != "shared-relay-a" {
		t.Fatalf("tunnel credentials = %v, want the public credential", runs)
	}
	if len(broker.accessRequests()) != accessUnappliedRetries+1 {
		t.Fatalf("access requests = %d, want the bounded retries", len(broker.accessRequests()))
	}
	awaitEvent(t, broker, "connection_succeeded", accessOutcomeUnapplied, relaySourcePublic)
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

// A renewal the broker answers with no relays means the connected relay has
// left the directory: the session recovers at once (here onto the public
// list, which still carries the relay) instead of retrying until expiry.
func TestAccessRenewalWithEmptyAnswerReconnects(t *testing.T) {
	clock := newFakeClock()
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, clock.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.useClock(clock)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	first := broker.lease("relay-a").credential
	broker.set(func() { broker.emptyAccess = true })

	// The renewer fires once, between a quarter and three quarters of the
	// lease; it must not wait for anything after the empty answer.
	for !hasNotice(e.sink, NoticeFailoverStarted) {
		waitUntil(t, "the lease renewer to wait or give up", func() bool {
			return clock.pending() > 0 || hasNotice(e.sink, NoticeFailoverStarted)
		})
		clock.Advance(30 * time.Second)
	}
	waitUntil(t, "the recovered tunnel", func() bool { return len(e.tunnelRuns()) == 2 })
	waitForStatus(t, e.Engine, StatusConnected)
	if runs := e.tunnelRuns(); runs[0] != first || runs[1] != "shared-relay-a" {
		t.Fatalf("tunnel credentials = %v, want the lease then the public credential", runs)
	}
	renewals := 0
	for _, request := range broker.accessRequests() {
		if request.RelayID == "relay-a" && request.Limit == 1 {
			renewals++
		}
	}
	if renewals != 1 {
		t.Fatalf("renewal requests = %d, want exactly one (no retry against a vanished relay)", renewals)
	}
	for _, notice := range e.sink.noticesSnapshot() {
		if notice.Kind == NoticeFailoverStarted && notice.Reason != "relay no longer listed" {
			t.Fatalf("failover reason = %q", notice.Reason)
		}
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

func hasNotice(sink *testSink, kind NoticeKind) bool {
	for _, notice := range sink.noticesSnapshot() {
		if notice.Kind == kind {
			return true
		}
	}
	return false
}

// advanceLeaseClock moves the fake clock forward in steps, letting the
// renewer react after each one (it always re-arms a timer when idle).
func advanceLeaseClock(t *testing.T, clock *fakeClock, total, step time.Duration) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed < total; elapsed += step {
		waitUntil(t, "the lease renewer to wait", func() bool { return clock.pending() > 0 })
		clock.Advance(step)
	}
	waitUntil(t, "the lease renewer to settle", func() bool { return clock.pending() > 0 })
}

// The renewal acceptance test: a connection survives three lease lifetimes
// on one tunnel with one stable credential and no expiry-induced recovery.
func TestAccessRenewalKeepsConnectionAcrossThreeLeaseLifetimes(t *testing.T) {
	clock := newFakeClock()
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, clock.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.useClock(clock)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	credential := broker.lease("relay-a").credential

	advanceLeaseClock(t, clock, 3*broker.ttl+broker.ttl/2, 30*time.Second)

	if state := e.State(); state.Status != StatusConnected {
		t.Fatalf("status after three lifetimes = %s", state.Status)
	}
	if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != credential {
		t.Fatalf("tunnel runs = %v, want the one tunnel on %s", runs, credential)
	}
	lease := broker.lease("relay-a")
	if lease.credential != credential || !lease.expiresAt.After(clock.Now()) {
		t.Fatalf("broker lease = %+v at %s, want %s still live", lease, clock.Now(), credential)
	}
	renewals := 0
	for _, req := range broker.accessRequests() {
		if req.RelayID == "relay-a" && req.Limit == 1 {
			renewals++
		}
	}
	if renewals < 6 || renewals > 12 {
		t.Fatalf("%d renewals over 3.5 lifetimes, want about two per lifetime", renewals)
	}
	if hasNotice(e.sink, NoticeFailoverStarted) || strings.Contains(e.sink.logLines(), "expired") {
		t.Fatalf("renewal caused a recovery:\n%s", e.sink.logLines())
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

// When renewals keep failing the lease expires on the live connection, and
// the session recovers through the ladder with a fresh credential.
func TestAccessExpiryWhileConnectedRecovers(t *testing.T) {
	clock := newFakeClock()
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, clock.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.useClock(clock)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	first := broker.lease("relay-a").credential
	broker.set(func() { broker.failRenewals = true })

	for !hasNotice(e.sink, NoticeFailoverStarted) {
		waitUntil(t, "the lease renewer to wait or give up", func() bool {
			return clock.pending() > 0 || hasNotice(e.sink, NoticeFailoverStarted)
		})
		clock.Advance(30 * time.Second)
	}
	waitUntil(t, "the recovered tunnel", func() bool { return len(e.tunnelRuns()) == 2 })
	waitForStatus(t, e.Engine, StatusConnected)

	runs := e.tunnelRuns()
	if runs[0] != first || runs[1] == first || runs[1] != broker.lease("relay-a").credential {
		t.Fatalf("tunnel credentials = %v, want %s then a fresh lease", runs, first)
	}
	for _, notice := range e.sink.noticesSnapshot() {
		if notice.Kind == NoticeFailoverStarted && notice.Reason != "relay access expired" {
			t.Fatalf("failover reason = %q", notice.Reason)
		}
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

func TestAccessDisabledWhileConnectedEndsTheSession(t *testing.T) {
	clock := newFakeClock()
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	broker := newFakeAccessBroker(t, clock.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.useClock(clock)

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	broker.set(func() { broker.disabled = true })
	waitUntil(t, "the lease renewer to wait", func() bool { return clock.pending() > 0 })
	go func() {
		for i := 0; i < 200 && e.State().Status == StatusConnected; i++ {
			clock.Advance(time.Minute)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	state := waitForStatus(t, e.Engine, StatusFailed)
	waitIdle(t, e.Engine)
	if state.LastError == nil || !strings.Contains(*state.LastError, "disabled") || !hasNotice(e.sink, NoticeAccessDenied) {
		t.Fatalf("lastError = %v, notices = %+v", state.LastError, e.sink.noticesSnapshot())
	}
	if len(e.tunnelRuns()) != 1 {
		t.Fatalf("a disabled installation reconnected: %v", e.tunnelRuns())
	}
}

func TestMobileHostRequiresInstallationKey(t *testing.T) {
	s, _, _ := mobileTestEngine(t)
	if err := s.validateMobile(); err != nil {
		t.Fatalf("validateMobile with a key = %v", err)
	}
	s.Mobile.Key = nil
	if err := s.validateMobile(); err == nil || !strings.Contains(err.Error(), "installation key") {
		t.Fatalf("validateMobile without a key = %v", err)
	}
}

func TestAccessTokenScopeFollowsTheIssuingBroker(t *testing.T) {
	defaults := brokerapi.DefaultBrokerURLs()
	if len(defaults) < 2 {
		t.Fatalf("expected several default fronts, got %v", defaults)
	}
	for _, front := range defaults[1:] {
		if accessTokenScopeFor(front) != accessTokenScopeFor(defaults[0]) {
			t.Fatalf("default front %s does not share the default scope", front)
		}
	}
	for _, custom := range []string{"https://custom.example/", "http://127.0.0.1:8080"} {
		if accessTokenScopeFor(custom) == accessTokenScopeFor(defaults[0]) {
			t.Fatalf("custom broker %s shares the default scope", custom)
		}
	}
	if accessTokenScopeFor("https://a.example/") == accessTokenScopeFor("https://b.example/") {
		t.Fatal("two custom brokers share a scope")
	}
}

// A token cached from one broker is never presented to another: switching
// brokers authenticates against the new one before any access request.
func TestAccessTokenIsNotSentToAnotherBroker(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "JP")}
	first := newFakeAccessBroker(t, time.Now, relays...)
	second := newFakeAccessBroker(t, time.Now, relays...)
	second.tokenPrefix = "second-"
	e := newAccessEngine(t, first, relays...)

	if err := e.Connect(first.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	_ = e.Disconnect()
	waitIdle(t, e.Engine)

	e.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		return discovery.Fetch{BrokerURL: second.srv.URL, Response: listOf(relays...)}, nil
	}
	if err := e.Connect(second.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	_ = e.Disconnect()
	waitIdle(t, e.Engine)

	second.mu.Lock()
	bearers := append([]string(nil), second.bearers...)
	second.mu.Unlock()
	if len(bearers) == 0 || second.auths.Load() != 1 {
		t.Fatalf("second broker: %d access requests, %d auths", len(bearers), second.auths.Load())
	}
	for _, bearer := range bearers {
		if !strings.HasPrefix(bearer, "Bearer second-") {
			t.Fatalf("second broker received %q, a token it did not issue", bearer)
		}
	}
	if runs := e.tunnelRuns(); len(runs) != 2 || runs[1] != second.lease("relay-a").credential {
		t.Fatalf("tunnel credentials = %v", runs)
	}
}

// mixedFleet is a lease-capable relay plus a public-only relay (the public
// list carries both, the access answer only the first); dials to dead hosts
// fail.
func mixedFleet(t *testing.T) (*fakeAccessBroker, *accessEngine, *atomic.Value) {
	t.Helper()
	leased := publicRelay("relay-a", "JP")
	leased.PublicHost = "198.51.100.1"
	public := publicRelay("relay-p", "JP")
	public.PublicHost = "198.51.100.2"
	broker := newFakeAccessBroker(t, time.Now, leased)
	e := newAccessEngine(t, broker, leased, public)
	dead := &atomic.Value{}
	dead.Store("")
	e.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		if host == dead.Load().(string) {
			return 0, fmt.Errorf("dial %s: connection refused", host)
		}
		return 1, nil
	}
	return broker, e, dead
}

// During the compatibility phase a public-only relay still carries the
// connect when every leased candidate fails.
func TestAccessFallsBackToPublicCandidatesWhenLeasedOnesFail(t *testing.T) {
	broker, e, dead := mixedFleet(t)
	dead.Store("198.51.100.1")

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != "shared-relay-p" {
		t.Fatalf("tunnel credentials = %v, want the public-only relay", runs)
	}
	if e.public.Load() != 1 {
		t.Fatalf("public list fetched %d times, want the race's answer reused", e.public.Load())
	}
	if info, ok := e.ActiveConnectionInfo(); !ok || info.Relay.ID != "relay-p" {
		t.Fatalf("active relay = %+v", info)
	}
	awaitEvent(t, broker, "connection_succeeded", accessOutcomeFallbackPublic, relaySourcePublic)
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

// A recovery asks the pinned front for access without racing; when its
// leased candidates fail it races for the public list and uses it.
func TestAccessRecoveryFallsBackToPublicCandidates(t *testing.T) {
	_, e, dead := mixedFleet(t)
	e.healthTick = 5 * time.Millisecond
	e.healthProbe = func(ctx context.Context, proxyPort int) error {
		if runs := e.tunnelRuns(); len(runs) > 0 && strings.HasPrefix(runs[len(runs)-1], "lease-") && dead.Load().(string) != "" {
			return fmt.Errorf("probe timeout")
		}
		return nil
	}

	if err := e.Connect("https://configured.example", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	if runs := e.tunnelRuns(); len(runs) != 1 || !strings.HasPrefix(runs[0], "lease-relay-a") {
		t.Fatalf("initial tunnel credentials = %v", runs)
	}
	dead.Store("198.51.100.1")
	waitUntil(t, "the public-only relay", func() bool {
		runs := e.tunnelRuns()
		return len(runs) >= 2 && runs[len(runs)-1] == "shared-relay-p"
	})
	waitForStatus(t, e.Engine, StatusConnected)
	if e.public.Load() != 2 {
		t.Fatalf("public list fetched %d times, want the initial race plus one recovery race", e.public.Load())
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

// An access answer whose relays the ladder would reject (here: a dedicated
// exit) is treated like an empty one (recovery shares this fetchAccess path),
// so a usable public relay still carries the connect.
func TestAccessFallsBackWhenNoLeasedRelayIsUsable(t *testing.T) {
	for name, target := range map[string]RelayTarget{"untargeted": {}, "country": {Country: "JP"}} {
		t.Run(name, func(t *testing.T) {
			leased := publicRelay("relay-a", "JP")
			public := publicRelay("relay-p", "JP")
			broker := newFakeAccessBroker(t, time.Now, leased)
			broker.mutateAccess = func(r *brokerapi.RelayDescriptor) { r.ExitMode = brokerapi.ExitModeDedicated }
			e := newAccessEngine(t, broker, leased, public)
			e.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
				e.public.Add(1)
				// The public list has only the public-only relay usable.
				dedicated := leased
				dedicated.ExitMode = brokerapi.ExitModeDedicated
				return discovery.Fetch{BrokerURL: broker.srv.URL, Response: listOf(dedicated, public)}, nil
			}

			if err := e.ConnectTarget("https://configured.example", target); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, e.Engine, StatusConnected)
			if runs := e.tunnelRuns(); len(runs) != 1 || runs[0] != "shared-relay-p" {
				t.Fatalf("tunnel credentials = %v, want the usable public relay", runs)
			}
			if len(broker.accessRequests()) != 1 {
				t.Fatalf("access requests = %d", len(broker.accessRequests()))
			}
			_ = e.Disconnect()
			waitIdle(t, e.Engine)
		})
	}
}
