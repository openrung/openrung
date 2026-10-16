// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

// leaseHarness drives the lease sync endpoints through the user harness's
// mux: a short poll hold, a controllable clock, and a registered relay.
type leaseHarness struct {
	*userTestHarness
	store *Store
	relay relay.Descriptor
}

func newLeaseHarness(t *testing.T) *leaseHarness {
	t.Helper()
	store := NewStore()
	h := &leaseHarness{userTestHarness: newUserTestHarness(t, store, Config{APIToken: "api-token"}), store: store}
	h.relay = registerLeaseRelay(t, store, 'P', "poll.example.com", h.now)
	return h
}

func (h *leaseHarness) poll(relayID, token, query string) (*httptest.ResponseRecorder, relay.LeaseSyncResponse) {
	h.t.Helper()
	rec := h.do(http.MethodGet, "/api/v1/relays/"+relayID+"/leases"+query, "", map[string]string{"Authorization": "Bearer " + token})
	var resp relay.LeaseSyncResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			h.t.Fatal(err)
		}
	}
	return rec, resp
}

func (h *leaseHarness) ack(relayID, token, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/api/v1/relays/"+relayID+"/leases/ack", body, map[string]string{"Authorization": "Bearer " + token})
}

// pollAsync starts a poll and returns a channel that yields its outcome and
// how long it took.
func (h *leaseHarness) pollAsync(query string) <-chan struct {
	rec     *httptest.ResponseRecorder
	resp    relay.LeaseSyncResponse
	elapsed time.Duration
} {
	out := make(chan struct {
		rec     *httptest.ResponseRecorder
		resp    relay.LeaseSyncResponse
		elapsed time.Duration
	}, 1)
	started := time.Now()
	go func() {
		rec, resp := h.poll(h.relay.ID, h.relay.LeaseToken, query)
		out <- struct {
			rec     *httptest.ResponseRecorder
			resp    relay.LeaseSyncResponse
			elapsed time.Duration
		}{rec, resp, time.Since(started)}
	}()
	return out
}

func TestLeasePollFirstPollReturnsImmediatelyEvenWhenEmpty(t *testing.T) {
	h := newLeaseHarness(t)
	started := time.Now()
	rec, resp := h.poll(h.relay.ID, h.relay.LeaseToken, "")
	if rec.Code != http.StatusOK || time.Since(started) > h.access.pollHold/2 {
		t.Fatalf("first poll: %d after %s: %s", rec.Code, time.Since(started), rec.Body.String())
	}
	if resp.Revision != 0 || len(resp.Leases) != 0 || !resp.ServerTime.Equal(h.now) {
		t.Fatalf("first poll body = %+v", resp)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	// The body always carries a leases array, never null, so relays can
	// reconcile without a nil check.
	if !strings.Contains(rec.Body.String(), `"leases":[]`) {
		t.Fatalf("empty set serialized as %s", rec.Body.String())
	}
}

func TestLeasePollHoldsOnEmptySetAtRevisionZero(t *testing.T) {
	h := newLeaseHarness(t)
	result := <-h.pollAsync("?after=0")
	if result.rec.Code != http.StatusOK || result.elapsed < h.access.pollHold {
		t.Fatalf("after=0 on an empty set answered in %s with %d, want a full hold", result.elapsed, result.rec.Code)
	}
	if result.resp.Revision != 0 || len(result.resp.Leases) != 0 {
		t.Fatalf("held poll body = %+v", result.resp)
	}
}

func TestLeasePollWakesOnIssueAndRevoke(t *testing.T) {
	h := newLeaseHarness(t)
	user := enrollLeaseUser(t, h.store, h.now)

	pending := h.pollAsync("?after=0")
	time.Sleep(50 * time.Millisecond)
	leases, err := h.access.issueLeases(context.Background(), user.ID, []string{h.relay.ID})
	if err != nil || len(leases) != 1 {
		t.Fatalf("issue: %v %v", leases, err)
	}
	result := <-pending
	if result.rec.Code != http.StatusOK || result.elapsed >= h.access.pollHold {
		t.Fatalf("poll after issue: %d in %s (hold %s)", result.rec.Code, result.elapsed, h.access.pollHold)
	}
	if result.resp.Revision != 1 || len(result.resp.Leases) != 1 || result.resp.Leases[0].ID != leases[0].ID || result.resp.Leases[0].Credential != leases[0].Credential || !result.resp.Leases[0].ExpiresAt.Equal(leases[0].ExpiresAt) {
		t.Fatalf("woken poll body = %+v, want lease %+v", result.resp, leases[0])
	}
	if strings.Contains(result.rec.Body.String(), user.ID) {
		t.Fatal("the relay response carried a user ID")
	}

	// Already-applied revision: holds, then returns the unchanged set.
	unchanged := <-h.pollAsync("?after=1")
	if unchanged.elapsed < h.access.pollHold || unchanged.resp.Revision != 1 {
		t.Fatalf("poll at the current revision = %+v after %s", unchanged.resp, unchanged.elapsed)
	}
	// A stale `after` answers immediately with the newer set.
	stale := <-h.pollAsync("?after=0")
	if stale.elapsed >= h.access.pollHold || stale.resp.Revision != 1 {
		t.Fatalf("stale poll = %+v after %s", stale.resp, stale.elapsed)
	}

	// Disabling the user through the operational API revokes and wakes.
	pending = h.pollAsync("?after=1")
	time.Sleep(50 * time.Millisecond)
	if rec := h.setStatus(user.ID, `{"status":"disabled","reason":"test"}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	result = <-pending
	if result.elapsed >= h.access.pollHold || result.resp.Revision != 2 || len(result.resp.Leases) != 0 {
		t.Fatalf("poll after revoke = %+v in %s", result.resp, result.elapsed)
	}
}

func TestLeasePollAndAckRefuseWrongTokenAndLegacyRows(t *testing.T) {
	h := newLeaseHarness(t)
	legacy, err := h.store.Register(validRegisterRequest(), h.now, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		relayID string
		token   string
	}{
		{"wrong token", h.relay.ID, "lease_" + strings.Repeat("0", 64)},
		{"missing token", h.relay.ID, ""},
		{"legacy identityless row", legacy.ID, "lease_" + strings.Repeat("0", 64)},
		{"unknown relay", "relay_" + strings.Repeat("f", 32), h.relay.LeaseToken},
		{"malformed relay id", "not-a-relay", h.relay.LeaseToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := h.poll(tc.relayID, tc.token, "")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"relay not found"`) {
				t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
			}
			rec = h.ack(tc.relayID, tc.token, `{"revision":1}`)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"relay not found"`) {
				t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if rec, _ := h.poll(h.relay.ID, h.relay.LeaseToken, "?after=-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative after: %d", rec.Code)
	}
	if rec, _ := h.poll(h.relay.ID, h.relay.LeaseToken, "?after=abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric after: %d", rec.Code)
	}
}

// Unauthenticated polls and abandoned holds leave nothing behind in the
// notifier: subscriptions exist only while an authenticated request waits.
func TestLeasePollRetainsNoStateForRefusedOrAbandonedRequests(t *testing.T) {
	h := newLeaseHarness(t)
	for i := 0; i < 50; i++ {
		id := "relay_" + strings.Repeat(fmt.Sprintf("%x", i%16), 32)
		if rec, _ := h.poll(id, "lease_"+strings.Repeat("0", 64), "?after=0"); rec.Code != http.StatusNotFound {
			t.Fatalf("poll %s: %d", id, rec.Code)
		}
	}
	if rec, _ := h.poll(h.relay.ID, "lease_wrong", "?after=0"); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if got := h.access.notifier.waiting(); got != 0 {
		t.Fatalf("%d notifier entries after refused polls", got)
	}
	// A hold that times out releases its subscription too, and a cancelled
	// request likewise.
	if rec, _ := h.poll(h.relay.ID, h.relay.LeaseToken, "?after=0"); rec.Code != http.StatusOK {
		t.Fatalf("held poll: %d", rec.Code)
	}
	if got := h.access.notifier.waiting(); got != 0 {
		t.Fatalf("%d notifier entries after a timed-out hold", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays/"+h.relay.ID+"/leases?after=0", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+h.relay.LeaseToken)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.server.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(50 * time.Millisecond)
	if got := h.access.notifier.waiting(); got != 1 {
		t.Fatalf("%d notifier entries while one authenticated poll waits, want 1", got)
	}
	cancel()
	<-done
	if got := h.access.notifier.waiting(); got != 0 {
		t.Fatalf("%d notifier entries after a cancelled hold", got)
	}
}

func TestLeaseAckRecordsAppliedRevision(t *testing.T) {
	h := newLeaseHarness(t)
	user := enrollLeaseUser(t, h.store, h.now)
	if _, err := h.access.issueLeases(context.Background(), user.ID, []string{h.relay.ID}); err != nil {
		t.Fatal(err)
	}
	applied := h.access.notifier.appliedC(h.relay.ID)
	defer applied.release()
	if rec := h.ack(h.relay.ID, h.relay.LeaseToken, `{"revision":1}`); rec.Code != http.StatusNoContent {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-applied.C:
	default:
		t.Fatal("ack did not wake applied waiters")
	}
	revisions, _ := h.store.AppliedLeaseRevisions(context.Background(), []string{h.relay.ID})
	if revisions[h.relay.ID] != 1 {
		t.Fatalf("applied = %v", revisions)
	}
	for _, body := range []string{`{"revision":-1}`, `{"revision":"1"}`, `{"revision":1,"extra":true}`, `{"revision":1}{"revision":2}`, ``} {
		if rec := h.ack(h.relay.ID, h.relay.LeaseToken, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("ack %q: %d", body, rec.Code)
		}
	}
	revisions, _ = h.store.AppliedLeaseRevisions(context.Background(), []string{h.relay.ID})
	if revisions[h.relay.ID] != 1 {
		t.Fatalf("bad acks changed applied to %v", revisions)
	}
}

func TestAdminLeaseLookupResolvesLabel(t *testing.T) {
	h := newLeaseHarness(t)
	user := enrollLeaseUser(t, h.store, h.now)
	leases, err := h.access.issueLeases(context.Background(), user.ID, []string{h.relay.ID})
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"Authorization": "Bearer api-token"}
	for _, path := range []string{"/admin/api/leases/" + leases[0].ID, "/admin/api/leases/lease-" + leases[0].ID} {
		rec := h.do(http.MethodGet, path, "", auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var view leaseView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.LeaseID != leases[0].ID || view.Label != "lease-"+leases[0].ID || view.UserID != user.ID || view.RelayID != h.relay.ID || view.Status != "live" || view.Revision != 1 || view.RevokedAt != nil {
			t.Fatalf("view = %+v", view)
		}
		if strings.Contains(rec.Body.String(), leases[0].Credential) {
			t.Fatal("the operator view leaked the credential")
		}
	}
	if rec := h.do(http.MethodGet, "/admin/api/leases/"+leases[0].ID, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/admin/api/leases/"+strings.Repeat("0", 32), "", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown lease: %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/admin/api/leases/nope", "", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed lease id: %d", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/admin/api/leases/"+leases[0].ID, "", auth); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := h.setStatus(user.ID, `{"status":"disabled","reason":"test"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec := h.do(http.MethodGet, "/admin/api/leases/"+leases[0].ID, "", auth)
	var view leaseView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Status != "revoked" || view.RevokedAt == nil || view.Revision != 2 {
		t.Fatalf("revoked view = %+v", view)
	}
}

// Through the real server: the ack and poll patterns win over the heartbeat
// prefix pattern, registration echoes the capability, and the heartbeat
// keeps it. A registration-token bearer is not a lease token, so the ack
// route answers 404 like the poll route rather than reaching the heartbeat
// handler (which would answer 401 for the wrong bearer).
func TestLeaseRoutesThroughNewServer(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), RegistrationToken: "reg-token", UserTokenKey: testUserTokenKey(), APIToken: "api-token"})
	priv, err := relay.ParseIdentitySeed(identityStoreSeedA)
	if err != nil {
		t.Fatal(err)
	}
	req := validRegisterRequest()
	req.AccessLeases = true
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = relay.SignIdentity(priv, req, time.Now().Add(relay.IdentityProofTTLDirect))
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer reg-token")
	server.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var registered relay.RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	desc := registered.Descriptor
	if !desc.LeaseCapable || !strings.Contains(rec.Body.String(), `"access_leases":true`) {
		t.Fatalf("registration did not echo the lease capability: %s", rec.Body.String())
	}

	do := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		server.ServeHTTP(rec, r)
		return rec
	}
	if rec := do(http.MethodGet, "/api/v1/relays/"+desc.ID+"/leases", desc.LeaseToken, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revision":0`) {
		t.Fatalf("poll through NewServer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/relays/"+desc.ID+"/leases/ack", desc.LeaseToken, `{"revision":0}`); rec.Code != http.StatusNoContent {
		t.Fatalf("ack through NewServer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/relays/"+desc.ID+"/leases/ack", "reg-token", `{"revision":0}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "relay not found") {
		t.Fatalf("ack with the registration bearer reached another handler: %d %s", rec.Code, rec.Body.String())
	}
	// The heartbeat itself still routes to the heartbeat handler and keeps
	// the capability it never carries.
	hb, _ := json.Marshal(relay.HeartbeatRequest{OK: true, LeaseToken: desc.LeaseToken})
	if rec := do(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", "reg-token", string(hb)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", desc.LeaseToken, string(hb)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat with the lease bearer: %d", rec.Code)
	}
	// Admin lease lookup is registered too.
	if rec := do(http.MethodGet, "/admin/api/leases/"+strings.Repeat("0", 32), "api-token", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "lease not found") {
		t.Fatalf("admin lease route: %d %s", rec.Code, rec.Body.String())
	}
}

// The capability is private registration state: a legacy identityless
// registration can never claim it, a heartbeat never changes it, and the
// public directory never carries it.
func TestLeaseCapabilityIsRegistrationOnly(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	legacyReq := validRegisterRequest()
	legacyReq.AccessLeases = true
	legacy, err := store.Register(legacyReq, now, 3*time.Minute)
	if err != nil || legacy.LeaseCapable {
		t.Fatalf("legacy registration lease_capable = %v, %v", legacy.LeaseCapable, err)
	}
	desc := registerLeaseRelay(t, store, 'Q', "q.example.com", now)
	renewed, err := store.Heartbeat(desc.ID, desc.LeaseToken, relay.NodeClassVolunteer, "", now.Add(time.Minute), 3*time.Minute)
	if err != nil || !renewed.LeaseCapable {
		t.Fatalf("heartbeat lost the capability: %v %v", renewed.LeaseCapable, err)
	}
	listed, err := store.List(now.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed {
		body, _ := json.Marshal(d)
		if bytes.Contains(body, []byte("lease")) {
			t.Fatalf("public descriptor carries lease state: %s", body)
		}
	}
	// Re-registering without the flag withdraws it.
	plain := signedIdentityRequest(t, identitySeed('Q'), func(r *relay.RegisterRequest) { r.PublicHost = "q.example.com" }, now.Add(2*time.Minute))
	again, err := store.Register(plain, now.Add(2*time.Minute), 3*time.Minute)
	if err != nil || again.LeaseCapable {
		t.Fatalf("re-registration without access_leases kept lease_capable: %v %v", again.LeaseCapable, err)
	}
}

func TestLeaseServiceAppliesConfiguredCapsAndTTL(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	access := newAccessService(store, Config{AccessLeaseTTL: 10 * time.Minute, MaxLeasesPerUser: 1})
	access.now = func() time.Time { return now }
	user := enrollLeaseUser(t, store, now)
	a, b := "relay_"+strings.Repeat("a", 32), "relay_"+strings.Repeat("b", 32)
	first, err := access.issueLeases(context.Background(), user.ID, []string{a})
	if err != nil || len(first) != 1 || !first[0].ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("issue = %+v, %v", first, err)
	}
	second, err := access.issueLeases(context.Background(), user.ID, []string{b})
	if err != nil || len(second) != 1 {
		t.Fatalf("second issue = %+v, %v", second, err)
	}
	if evicted, _ := store.LeaseByID(context.Background(), first[0].ID); evicted.RevokedAt.IsZero() {
		t.Fatal("the per-user cap of 1 did not evict the first lease")
	}
	if _, err := access.issueLeases(context.Background(), "user_"+strings.Repeat("0", 32), []string{a}); err == nil {
		t.Fatal("issuing for an unknown user succeeded")
	}
	if newAccessService(relayOnlyStore{store}, Config{}) != nil {
		t.Fatal("a relay-only store must yield no access service")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an out-of-range lease TTL must panic at construction")
		}
	}()
	newAccessService(store, Config{AccessLeaseTTL: time.Second})
}

var _ = brokerapi.UserStatusEnabled
