// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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
	mux.HandleFunc("POST /api/v1/access", h.handler.handler())
	return h
}

// addRelay registers a relay for the whole test (a day-long registration
// lease, so clock advances never expire it) with a country code.
func (h *accessHarness) addRelay(name string, seed byte, country string, mutate func(*relay.RegisterRequest)) relay.Descriptor {
	h.t.Helper()
	req := signedIdentityRequest(h.t, identitySeed(seed), func(r *relay.RegisterRequest) {
		r.PublicHost = name + ".example.com"
		r.AccessLeases = true
		if mutate != nil {
			mutate(r)
		}
	}, h.now)
	desc, err := h.store.Register(req, h.now, 24*time.Hour)
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
