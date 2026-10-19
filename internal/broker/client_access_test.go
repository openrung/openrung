// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

// accessHarness serves POST /api/v1/access beside the user and lease routes
// of the user harness, over an in-memory store and a controllable clock.
type accessHarness struct {
	*userTestHarness
	store   *Store
	handler *clientAccessHandler
	relays  map[string]relay.Descriptor
}

func newAccessHarness(t *testing.T) *accessHarness {
	t.Helper()
	store := NewStore()
	h := &accessHarness{userTestHarness: newUserTestHarness(t, store, Config{APIToken: "api-token"}), store: store, relays: map[string]relay.Descriptor{}}
	h.handler = newClientAccessHandler(store, h.users, h.access, newSigner(testSigningSeed()))
	h.handler.applyTimeout = 300 * time.Millisecond
	mux := h.server.(*http.ServeMux)
	mux.HandleFunc("POST /api/v1/access", h.handler.counted(h.handler.handler()))
	return h
}

// addRelay registers a relay for the whole test (a day-long registration
// lease, so clock advances never expire it) with a country code.
func (h *accessHarness) addRelay(name string, seed byte, country string, mutate func(*relay.RegisterRequest)) relay.Descriptor {
	h.t.Helper()
	return h.addRelayTTL(name, seed, country, 24*time.Hour, mutate)
}

// addRelayTTL is addRelay with an explicit registration lease, for tests that
// let the relay expire from the directory.
func (h *accessHarness) addRelayTTL(name string, seed byte, country string, ttl time.Duration, mutate func(*relay.RegisterRequest)) relay.Descriptor {
	h.t.Helper()
	req := signedIdentityRequest(h.t, identitySeed(seed), func(r *relay.RegisterRequest) {
		r.PublicHost = name + ".example.com"
		r.AccessLeases = true
		if mutate != nil {
			mutate(r)
		}
	}, h.now)
	desc, err := h.store.Register(req, h.now, ttl)
	if err != nil {
		h.t.Fatalf("register %s: %v", name, err)
	}
	if country != "" {
		if err := h.store.UpdateGeo(desc.ID, desc.LeaseToken, relay.GeoLocation{Country: country, CountryCode: country}); err != nil {
			h.t.Fatal(err)
		}
	}
	h.relays[name] = desc
	return desc
}

// autoAck plays a lease-capable relay: it applies every desired-set change
// as soon as the broker announces it. It stops with the test.
func (h *accessHarness) autoAck(desc relay.Descriptor) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	h.t.Cleanup(func() { close(stop); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			sub := h.access.notifier.changedC(desc.ID)
			set, err := h.store.RelayLeaseSet(context.Background(), desc.ID, desc.LeaseToken, h.now)
			if err == nil {
				if err := h.store.AckLeaseRevision(context.Background(), desc.ID, desc.LeaseToken, set.Revision, h.now); err == nil {
					h.access.notifier.notifyApplied(desc.ID)
				}
			}
			select {
			case <-sub.C:
			case <-stop:
				sub.release()
				return
			}
			sub.release()
		}
	}()
}

func (h *accessHarness) token() (string, string) {
	h.t.Helper()
	code, auth, rec := h.auth(newUserKey(h.t))
	if code != http.StatusOK {
		h.t.Fatalf("auth: %d %s", code, rec.Body.String())
	}
	return auth.AccessToken, auth.UserID
}

func (h *accessHarness) request(token, body string) (*httptest.ResponseRecorder, brokerapi.AccessResponse) {
	h.t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	rec := h.do(http.MethodPost, "/api/v1/access", body, headers)
	var resp brokerapi.AccessResponse
	if rec.Code == http.StatusOK {
		verifyAccessSignature(h.t, rec)
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			h.t.Fatal(err)
		}
	}
	return rec, resp
}

// rawRequest is request without the 200-body verification, for answers that
// are deliberately not written (a cancelled request leaves the recorder at
// its 200 default with an empty body).
func (h *accessHarness) rawRequest(token, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return h.do(http.MethodPost, "/api/v1/access", body, headers)
}

// verifyAccessSignature checks the detached signature over the exact body
// and the channel inside it.
func verifyAccessSignature(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	fields := strings.Split(rec.Header().Get(signatureHeader), ";")
	if len(fields) != 3 || fields[0] != "ed25519" {
		t.Fatalf("signature header = %q", rec.Header().Get(signatureHeader))
	}
	sig, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.NewKeyFromSeed(testSigningSeed()).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, rec.Body.Bytes(), sig) {
		t.Fatal("access body does not verify under the relay-list key")
	}
	var envelope struct {
		Channel string `json:"channel"`
		Limit   *int   `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope.Channel != brokerapi.ChannelAccess || envelope.Limit != nil {
		t.Fatalf("access envelope = %+v (%v): %s", envelope, err, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func (h *accessHarness) liveLeases(userID string) []Lease {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return h.store.liveLeasesForUserLocked(userID, h.now)
}

func TestAccessIssuesSignedLeasesOnEligibleRelays(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	b := h.addRelay("b", 'b', "DE", nil)
	h.autoAck(a)
	h.autoAck(b)
	// Not lease-capable, tunnel-transport and identityless relays are never
	// handed out on the access channel.
	h.addRelay("incapable", 'c', "JP", func(r *relay.RegisterRequest) { r.AccessLeases = false })
	h.addRelay("tunnel", 'd', "JP", func(r *relay.RegisterRequest) { r.Transport = relay.TransportTunnel; r.ExitHost = "198.51.100.7" })
	legacy := validRegisterRequest()
	legacy.PublicHost = "legacy.example.com"
	legacy.AccessLeases = true
	if _, err := h.store.Register(legacy, h.now, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	token, userID := h.token()
	rec, resp := h.request(token, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("access: %d %s", rec.Code, rec.Body.String())
	}
	if len(resp.Relays) != 2 || resp.Count != 2 {
		t.Fatalf("access relays = %+v", resp.Relays)
	}
	leases := h.liveLeases(userID)
	credentials := map[string]string{}
	for _, lease := range leases {
		credentials[lease.RelayID] = lease.Credential
	}
	for _, desc := range resp.Relays {
		if desc.ID != a.ID && desc.ID != b.ID {
			t.Fatalf("ineligible relay %s handed out", desc.ID)
		}
		if desc.ClientID != credentials[desc.ID] || desc.ClientID == a.ClientID || !uuidV4.MatchString(desc.ClientID) {
			t.Fatalf("relay %s client_id = %q, want its lease credential %q", desc.ID, desc.ClientID, credentials[desc.ID])
		}
	}
	if !resp.LeaseExpiresAt.Equal(h.now.Add(defaultAccessLeaseTTL)) || !resp.ServerTime.Equal(h.now) {
		t.Fatalf("lease_expires_at = %s, server_time = %s", resp.LeaseExpiresAt, resp.ServerTime)
	}
	if strings.Contains(rec.Body.String(), userID) {
		t.Fatal("the access body carried the user ID")
	}
}

func TestAccessWithholdsRelaysWhoseProbeFailed(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	b := h.addRelay("b", 'b', "JP", nil)
	h.autoAck(a)
	h.autoAck(b)
	if err := h.store.UpdateReachability(b.ID, b.LeaseToken, false, h.now); err != nil {
		t.Fatal(err)
	}
	token, _ := h.token()

	// With probing off a stored failure is stale and changes nothing.
	rec, resp := h.request(token, `{}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 2 {
		t.Fatalf("access with probing off: %d %+v, want both relays", rec.Code, resp.Relays)
	}

	h.handler.withholdUnreachable = true
	rec, resp = h.request(token, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("access: %d %s", rec.Code, rec.Body.String())
	}
	if len(resp.Relays) != 1 || resp.Relays[0].ID != a.ID {
		t.Fatalf("access relays = %+v, want only %s", resp.Relays, a.ID)
	}
	// Naming the withheld relay directly does not bypass the probe result.
	rec, resp = h.request(token, `{"relay_id":"`+b.ID+`"}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 0 {
		t.Fatalf("targeted access to an unreachable relay: %d %+v", rec.Code, resp.Relays)
	}

	// A later successful probe makes it eligible again.
	if err := h.store.UpdateReachability(b.ID, b.LeaseToken, true, h.now); err != nil {
		t.Fatal(err)
	}
	rec, resp = h.request(token, `{"relay_id":"`+b.ID+`"}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 1 {
		t.Fatalf("access after a successful probe: %d %+v", rec.Code, resp.Relays)
	}
}

func TestAccessTargetedRequestIssuesOnlyForTheTarget(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	b := h.addRelay("b", 'b', "DE", nil)
	h.autoAck(a)
	h.autoAck(b)
	token, userID := h.token()

	rec, resp := h.request(token, `{"limit":1,"relay_id":"`+b.ID+`"}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 1 || resp.Relays[0].ID != b.ID {
		t.Fatalf("relay target: %d %s", rec.Code, rec.Body.String())
	}
	if leases := h.liveLeases(userID); len(leases) != 1 || leases[0].RelayID != b.ID {
		t.Fatalf("leases after a relay-targeted request = %+v", leases)
	}

	rec, resp = h.request(token, `{"country":"jp"}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 1 || resp.Relays[0].ID != a.ID {
		t.Fatalf("country target: %d %s", rec.Code, rec.Body.String())
	}
	if leases := h.liveLeases(userID); len(leases) != 2 {
		t.Fatalf("leases after a country-targeted request = %+v", leases)
	}

	// No eligible relay for the target: 200 with an empty list, nothing issued.
	for _, body := range []string{`{"country":"FR"}`, `{"relay_id":"relay_ffffffffffffffffffffffffffffffff"}`, `{"relay_id":"not a relay id"}`} {
		rec, resp = h.request(token, body)
		if rec.Code != http.StatusOK || len(resp.Relays) != 0 || !strings.Contains(rec.Body.String(), `"relays":[]`) || strings.Contains(rec.Body.String(), "lease_expires_at") {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if leases := h.liveLeases(userID); len(leases) != 2 {
		t.Fatalf("an empty answer issued leases: %+v", leases)
	}
}

func TestAccessRenewalKeepsTheCredentialAndExtendsExpiry(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	h.autoAck(a)
	token, _ := h.token()

	_, first := h.request(token, `{}`)
	if len(first.Relays) != 1 {
		t.Fatalf("first access = %+v", first)
	}
	for i := 1; i <= 3; i++ {
		h.now = h.now.Add(defaultAccessLeaseTTL / 2)
		rec, renewed := h.request(token, `{"limit":1,"relay_id":"`+a.ID+`"}`)
		if rec.Code != http.StatusOK || len(renewed.Relays) != 1 {
			t.Fatalf("renewal %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if renewed.Relays[0].ClientID != first.Relays[0].ClientID {
			t.Fatalf("renewal %d changed the credential", i)
		}
		if !renewed.LeaseExpiresAt.Equal(h.now.Add(defaultAccessLeaseTTL)) {
			t.Fatalf("renewal %d expiry = %s, want %s", i, renewed.LeaseExpiresAt, h.now.Add(defaultAccessLeaseTTL))
		}
	}
}

func TestAccessUnappliedIssuanceIs503ThenRetrySucceeds(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	token, userID := h.token()

	rec, _ := h.request(token, `{}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("unacknowledged issuance: %d Retry-After %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if rec.Header().Get(signatureHeader) != "" {
		t.Fatal("an error answer was signed")
	}
	issued := h.liveLeases(userID)
	if len(issued) != 1 {
		t.Fatalf("leases after 503 = %+v", issued)
	}
	if h.access.notifier.waiting() != 0 {
		t.Fatalf("ack wait left %d subscriptions behind", h.access.notifier.waiting())
	}

	h.autoAck(a)
	rec, resp := h.request(token, `{}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 1 || resp.Relays[0].ClientID != issued[0].Credential {
		t.Fatalf("retry: %d %s (want the persisted credential)", rec.Code, rec.Body.String())
	}
}

func TestAccessReturnsOnlyAcknowledgedRelays(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	b := h.addRelay("b", 'b', "DE", nil)
	h.autoAck(a)
	token, _ := h.token()
	rec, resp := h.request(token, `{}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 1 || resp.Relays[0].ID != a.ID {
		t.Fatalf("partial ack: %d %s (b=%s)", rec.Code, rec.Body.String(), b.ID)
	}
	if resp.Relays[0].ClientID == "" || resp.LeaseExpiresAt.IsZero() {
		t.Fatalf("partial answer lacks the credential or expiry: %+v", resp)
	}
	// The applied subset is the answer: a silent relay beside an acknowledged
	// one never triggers the extended wait.
	if got := h.handler.stats.snapshot(); got.extended != 0 || got.granted != 1 {
		t.Fatalf("stats after a partial answer = %+v", got)
	}
}

// snapshot copies the current window's counters for assertions.
func (s *accessStats) snapshot() accessCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessCounts
}

// ackOnce plays a relay that applies the current desired set exactly once,
// now: the manual counterpart of autoAck for tests that time the ack.
func (h *accessHarness) ackOnce(desc relay.Descriptor) {
	set, err := h.store.RelayLeaseSet(context.Background(), desc.ID, desc.LeaseToken, h.now)
	if err != nil {
		h.t.Error(err)
		return
	}
	if err := h.store.AckLeaseRevision(context.Background(), desc.ID, desc.LeaseToken, set.Revision, h.now); err != nil {
		h.t.Error(err)
		return
	}
	h.access.notifier.notifyApplied(desc.ID)
}

// captureLogs routes slog's default logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestAccessSlowButAliveRelayGetsOneExtendedWait(t *testing.T) {
	t.Run("acknowledges during the extension", func(t *testing.T) {
		h := newAccessHarness(t)
		a := h.addRelay("a", 'a', "JP", nil)
		token, _ := h.token()
		// The relay heartbeated just now (registration counts), so it is alive;
		// its ack arrives after the first wait has elapsed.
		timer := time.AfterFunc(h.handler.applyTimeout+50*time.Millisecond, func() { h.ackOnce(a) })
		defer timer.Stop()
		rec, resp := h.request(token, `{}`)
		if rec.Code != http.StatusOK || len(resp.Relays) != 1 || resp.Relays[0].ID != a.ID {
			t.Fatalf("slow ack: %d %s", rec.Code, rec.Body.String())
		}
		if got := h.handler.stats.snapshot(); got.extended != 1 || got.granted != 1 || got.unapplied != 0 {
			t.Fatalf("stats = %+v, want one extension and a grant", got)
		}
	})
	t.Run("never acknowledges", func(t *testing.T) {
		h := newAccessHarness(t)
		h.addRelay("a", 'a', "JP", nil)
		token, _ := h.token()
		started := time.Now()
		rec, _ := h.request(token, `{}`)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("alive but silent relay: %d %s", rec.Code, rec.Body.String())
		}
		// Bounded: exactly one extension, not an open-ended wait.
		if elapsed := time.Since(started); elapsed < 2*h.handler.applyTimeout || elapsed > 6*h.handler.applyTimeout {
			t.Fatalf("waited %s, want about twice the apply timeout (%s)", elapsed, h.handler.applyTimeout)
		}
		if got := h.handler.stats.snapshot(); got.extended != 1 || got.unapplied != 1 {
			t.Fatalf("stats = %+v, want one extension and one unapplied", got)
		}
		if h.access.notifier.waiting() != 0 {
			t.Fatalf("ack waits left %d subscriptions behind", h.access.notifier.waiting())
		}
	})
}

func TestAccessRelayWithStaleHeartbeatIs503WithoutExtending(t *testing.T) {
	h := newAccessHarness(t)
	h.addRelay("a", 'a', "JP", nil)
	token, _ := h.token()
	// Still in the directory (a day-long registration), but it has missed a
	// heartbeat: it is treated as gone, and nobody waits a second time for it.
	h.now = h.now.Add(relayHeartbeatInterval + time.Second)
	started := time.Now()
	rec, _ := h.request(token, `{}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("stale relay: %d %s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 2*h.handler.applyTimeout {
		t.Fatalf("waited %s for a relay that missed its heartbeat", elapsed)
	}
	if got := h.handler.stats.snapshot(); got.extended != 0 || got.unapplied != 1 {
		t.Fatalf("stats = %+v, want no extension", got)
	}
}

// relayLookupErrorStore fails RelayByID the way a store does when the
// database is unreachable; the lease store underneath keeps working.
type relayLookupErrorStore struct {
	RelayStore
	err error
}

func (s relayLookupErrorStore) RelayByID(string, time.Time) (relay.Descriptor, error) {
	return relay.Descriptor{}, s.err
}

func TestAccessHeartbeatLookupFailureIsAStoreError(t *testing.T) {
	h := newAccessHarness(t)
	h.addRelay("a", 'a', "JP", nil)
	token, _ := h.token()
	h.handler.store = relayLookupErrorStore{RelayStore: h.store, err: errors.New("database unreachable")}
	logs := captureLogs(t)

	started := time.Now()
	rec := h.rawRequest(token, `{}`)
	// Not the retryable "unapplied" answer: the generic 503 with the failure
	// logged, and no second wait on a store that cannot be read.
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("heartbeat lookup failure: %d Retry-After %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 2*h.handler.applyTimeout {
		t.Fatalf("waited %s after the lookup failed", elapsed)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "database unreachable") {
		t.Fatalf("expected the store failure in the error log, got:\n%s", logs.String())
	}
	if got := h.handler.stats.snapshot(); got.failed != 1 || got.unapplied != 0 || got.extended != 0 {
		t.Fatalf("stats = %+v, want one error and no extension", got)
	}
}

func TestAccessRenewalForRelayGoneFromDirectoryIsEmpty(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelayTTL("a", 'a', "JP", 2*time.Minute, nil)
	h.autoAck(a)
	token, userID := h.token()
	rec, first := h.request(token, `{}`)
	if rec.Code != http.StatusOK || len(first.Relays) != 1 {
		t.Fatalf("first access: %d %s", rec.Code, rec.Body.String())
	}
	issued := h.liveLeases(userID)

	// The relay's registration expires; its lease is still live.
	h.now = h.now.Add(3 * time.Minute)
	rec, renewal := h.request(token, `{"limit":1,"relay_id":"`+a.ID+`"}`)
	if rec.Code != http.StatusOK || len(renewal.Relays) != 0 || renewal.Count != 0 || renewal.Channel != brokerapi.ChannelAccess {
		t.Fatalf("renewal against a vanished relay: %d %s (want an empty signed 200)", rec.Code, rec.Body.String())
	}
	if !renewal.LeaseExpiresAt.IsZero() {
		t.Fatalf("empty answer carries a lease expiry: %s", renewal.LeaseExpiresAt)
	}
	// Nothing was issued or extended for it.
	if after := h.liveLeases(userID); len(after) != len(issued) || !after[0].ExpiresAt.Equal(issued[0].ExpiresAt) {
		t.Fatalf("leases changed by the empty answer: before %+v after %+v", issued, after)
	}
	if got := h.handler.stats.snapshot(); got.empty != 1 || got.granted != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

// issueErrorStore fails IssueLeases the way a store does when the request
// context ends mid-transaction (the error wraps the cancellation).
type issueErrorStore struct {
	AccessStore
	err error
}

func (s issueErrorStore) IssueLeases(ctx context.Context, req LeaseIssueRequest, now time.Time) (LeaseIssueResult, error) {
	if s.err != nil {
		return LeaseIssueResult{}, s.err
	}
	return s.AccessStore.IssueLeases(ctx, req, now)
}

func TestAccessCancelledRequestIsLoggedAtInfo(t *testing.T) {
	h := newAccessHarness(t)
	h.addRelay("a", 'a', "JP", nil)
	token, _ := h.token()
	logs := captureLogs(t)

	t.Run("cancelled context", func(t *testing.T) {
		logs.Reset()
		h.access.store = issueErrorStore{AccessStore: h.store, err: context.Canceled}
		defer func() { h.access.store = h.store }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/access", strings.NewReader(`{}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.server.ServeHTTP(rec, req)
		if strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("a client cancel was logged as an error:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "cancelled by the client") {
			t.Fatalf("expected an INFO cancellation line, got:\n%s", logs.String())
		}
		if got := h.handler.stats.snapshot(); got.cancelled != 1 || got.failed != 0 {
			t.Fatalf("stats = %+v, want one cancellation", got)
		}
	})
	t.Run("wrapped cancellation on a live request", func(t *testing.T) {
		logs.Reset()
		h.access.store = issueErrorStore{AccessStore: h.store, err: errors.New("begin: " + context.Canceled.Error())}
		defer func() { h.access.store = h.store }()
		// A plain error that merely mentions cancellation is a store failure.
		rec := h.rawRequest(token, `{}`)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("store failure: %d, logs:\n%s", rec.Code, logs.String())
		}
		logs.Reset()
		h.access.store = issueErrorStore{AccessStore: h.store, err: fmt.Errorf("begin: %w", context.Canceled)}
		rec = h.rawRequest(token, `{}`)
		// The caller is still connected, so it gets the retryable refusal;
		// only the log level changes.
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
			t.Fatalf("wrapped cancellation on a live request: %d %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "cancelled by the client") {
			t.Fatalf("wrapped cancellation logs:\n%s", logs.String())
		}
	})
	t.Run("store failure stays an error", func(t *testing.T) {
		logs.Reset()
		h.access.store = issueErrorStore{AccessStore: h.store, err: errors.New("disk on fire")}
		defer func() { h.access.store = h.store }()
		rec, _ := h.request(token, `{}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("store failure: %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "could not issue access leases") {
			t.Fatalf("store failure logs:\n%s", logs.String())
		}
	})
}

func TestAccessSummaryLogsCountsPerInterval(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	h.autoAck(a)
	h.handler.stats.every = time.Minute
	token, _ := h.token()
	logs := captureLogs(t)

	h.request(token, `{}`)                        // granted
	h.request(token, `{"country":"ZZ"}`)          // empty
	h.request("", `{}`)                           // unauthorized
	h.request(token, `{"limit":0,"country":"X"}`) // bad request
	if strings.Contains(logs.String(), "access requests") {
		t.Fatalf("summary logged before the interval elapsed:\n%s", logs.String())
	}
	h.now = h.now.Add(2 * time.Minute)
	h.request(token, `{}`) // granted; also closes the window
	line := ""
	for _, candidate := range strings.Split(logs.String(), "\n") {
		if strings.Contains(candidate, "access requests") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("no summary line after the interval:\n%s", logs.String())
	}
	for _, want := range []string{"level=INFO", "granted=2", "empty=1", "unauthorized=1", "bad_request=1", "unapplied=0", "denied=0", "cancelled=0", "window=2m0s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary line lacks %q: %s", want, line)
		}
	}
	if got := h.handler.stats.snapshot(); got.granted != 0 || got.empty != 0 {
		t.Fatalf("counters not reset after the summary: %+v", got)
	}
}

func TestAccessRefusals(t *testing.T) {
	h := newAccessHarness(t)
	a := h.addRelay("a", 'a', "JP", nil)
	h.autoAck(a)
	token, userID := h.token()

	if rec, _ := h.request("", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec, _ := h.request(token+"x", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered token: %d", rec.Code)
	}
	for _, body := range []string{`nope`, `{"limit":6}`, `{"limit":-1}`, `{"extra":1}`, `{} {}`, `{"relay_id":"` + a.ID + `","country":"JP"}`, `{"country":"JPN"}`, `{"country":"J1"}`} {
		if rec, _ := h.request(token, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	if rec := h.setStatus(userID, `{"status":"disabled","reason":"test"}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := h.request(token, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled user: %d %s", rec.Code, rec.Body.String())
	}
	if leases := h.liveLeases(userID); len(leases) != 0 {
		t.Fatalf("a disabled user holds leases: %+v", leases)
	}

	// A token for a user the store does not know (a wiped store) is 401.
	h.now = h.now.Add(time.Minute)
	ghost, _, err := h.users.tokens.issue("user_00000000000000000000000000000000", h.now)
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := h.request(ghost, `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", rec.Code)
	}
	// An expired token is 401.
	h.now = h.now.Add(defaultAccessTokenTTL + time.Minute)
	if rec, _ := h.request(token, `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", rec.Code)
	}
}

func TestAccessRateKeySeparatesUsersBehindOneAddress(t *testing.T) {
	h := newAccessHarness(t)
	tokenA, _ := h.token()
	tokenB, _ := h.token()
	key := h.handler.rateKey(newClientIPResolver(nil))
	req := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/access", nil)
		r.RemoteAddr = "203.0.113.10:4000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}
	ka, kb, none := key(req(tokenA)), key(req(tokenB)), key(req(""))
	if ka == kb || ka == none || none != "203.0.113.10" || !strings.HasPrefix(ka, "203.0.113.10\x00") {
		t.Fatalf("rate keys a=%q b=%q none=%q", ka, kb, none)
	}
	if key(req(tokenA)) != ka {
		t.Fatal("rate key is not stable for one user")
	}
}

func TestAccessRouteRegisteredOnlyWithUsersAndLeases(t *testing.T) {
	post := func(server http.Handler) int {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/access", strings.NewReader(`{}`)))
		return rec.Code
	}
	if code := post(NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), UserTokenKey: testUserTokenKey()})); code != http.StatusUnauthorized {
		t.Fatalf("configured broker: %d, want 401 from the access handler", code)
	}
	if code := post(NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("broker without a token key: %d", code)
	}
}
