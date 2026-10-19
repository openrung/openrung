package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

func testUserTokenKey() []byte { return bytes.Repeat([]byte{0x5a}, accessTokenKeyBytes) }

func newUserKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// userTestHarness drives the user endpoints through the real mux with a
// controllable clock and source address.
type userTestHarness struct {
	t      *testing.T
	server http.Handler
	users  *userAuthService
	access *accessService
	now    time.Time
	source string
}

func newUserTestHarness(t *testing.T, store RelayStore, cfg Config) *userTestHarness {
	t.Helper()
	if cfg.SigningSeed == nil {
		cfg.SigningSeed = testSigningSeed()
	}
	if cfg.UserTokenKey == nil {
		cfg.UserTokenKey = testUserTokenKey()
	}
	h := &userTestHarness{t: t, now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), source: "203.0.113.10:4000"}
	// Build the service the server will build, with the test clock, by
	// constructing the server and reaching into its service through a second
	// construction sharing the same store: simpler is to build the server and
	// then swap the clock on the service it holds, so expose it.
	h.users = newUserAuthService(store, cfg, newClientIPResolver(nil))
	h.users.now = func() time.Time { return h.now }
	h.access = newAccessService(store, cfg)
	h.access.now = func() time.Time { return h.now }
	h.access.pollHold = 300 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/challenge", h.users.challengeHandler())
	mux.HandleFunc("POST /api/v1/users/auth", h.users.authHandler())
	mux.HandleFunc("GET /api/v1/relays/{id}/leases", h.access.leasePollHandler())
	mux.HandleFunc("POST /api/v1/relays/{id}/leases/ack", h.access.leaseAckHandler())
	mux.HandleFunc("/admin/api/users/{id}", adminUserHandler(h.users.store, cfg.APIToken))
	mux.HandleFunc("/admin/api/users/{id}/status", adminUserStatusHandler(h.access.setUserStatus, cfg.APIToken, func(*http.Request) string { return "test" }))
	mux.HandleFunc("/admin/api/leases/{id}", h.access.adminLeaseHandler(cfg.APIToken))
	h.server = mux
	return h
}

func (h *userTestHarness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = h.source
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	recorder := httptest.NewRecorder()
	h.server.ServeHTTP(recorder, req)
	return recorder
}

func (h *userTestHarness) challenge() string {
	h.t.Helper()
	recorder := h.do(http.MethodPost, "/api/v1/users/challenge", "", nil)
	if recorder.Code != http.StatusCreated {
		h.t.Fatalf("challenge: %d %s", recorder.Code, recorder.Body.String())
	}
	if cc := recorder.Header().Get("Cache-Control"); cc != "no-store" {
		h.t.Fatalf("challenge Cache-Control = %q", cc)
	}
	var out brokerapi.UserChallenge
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	if !brokerapi.ValidUserChallenge(out.Challenge) || !out.ExpiresAt.Equal(h.now.Add(userChallengeTTL)) {
		h.t.Fatalf("challenge response %+v", out)
	}
	return out.Challenge
}

// auth signs a fresh challenge with key and posts it.
func (h *userTestHarness) auth(key *ecdsa.PrivateKey) (int, brokerapi.UserAuthResponse, *httptest.ResponseRecorder) {
	h.t.Helper()
	request, err := brokerapi.SignUserAuth(key, h.challenge())
	if err != nil {
		h.t.Fatal(err)
	}
	return h.authRequest(request)
}

func (h *userTestHarness) authRequest(request brokerapi.UserAuthRequest) (int, brokerapi.UserAuthResponse, *httptest.ResponseRecorder) {
	h.t.Helper()
	payload, _ := json.Marshal(request)
	recorder := h.do(http.MethodPost, "/api/v1/users/auth", string(payload), nil)
	var out brokerapi.UserAuthResponse
	if recorder.Code == http.StatusOK || recorder.Code == http.StatusForbidden {
		_ = json.Unmarshal(recorder.Body.Bytes(), &out)
	}
	return recorder.Code, out, recorder
}

func (h *userTestHarness) setStatus(id, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPut, "/admin/api/users/"+id+"/status", body, map[string]string{"Authorization": "Bearer api-token"})
}

func TestUserAuthEnrollsOnceAndReturnsStableID(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{})
	key := newUserKey(t)

	code, first, recorder := h.auth(key)
	if code != http.StatusOK {
		t.Fatalf("first auth: %d %s", code, recorder.Body.String())
	}
	if !wellFormedUserID(first.UserID) || first.Status != brokerapi.UserStatusEnabled || first.AccessToken == "" {
		t.Fatalf("first auth response %s", first)
	}
	if !first.TokenExpiresAt.Equal(h.now.Add(defaultAccessTokenTTL)) {
		t.Fatalf("token expires %s, want %s", first.TokenExpiresAt, h.now.Add(defaultAccessTokenTTL))
	}
	if userID, ok := h.users.tokens.verify(first.AccessToken, h.now); !ok || userID != first.UserID {
		t.Fatalf("issued token does not verify to its user: %q %v", userID, ok)
	}

	h.now = h.now.Add(2 * time.Hour)
	code, second, recorder := h.auth(key)
	if code != http.StatusOK {
		t.Fatalf("second auth: %d %s", code, recorder.Body.String())
	}
	if second.UserID != first.UserID {
		t.Fatalf("second auth minted %q, want the stable %q", second.UserID, first.UserID)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("second auth reissued the identical token")
	}
	user, err := h.users.store.UserByID(context.Background(), first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if !user.LastAuthAt.Equal(h.now) || !user.CreatedAt.Equal(h.now.Add(-2*time.Hour)) {
		t.Fatalf("user timeline = created %s last %s", user.CreatedAt, user.LastAuthAt)
	}

	// A different key is a different user.
	code, other, _ := h.auth(newUserKey(t))
	if code != http.StatusOK || other.UserID == first.UserID {
		t.Fatalf("other key: %d %s", code, other)
	}
}

func TestUserAuthChallengeIsSingleUseAndExpires(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{})
	key := newUserKey(t)

	request, err := brokerapi.SignUserAuth(key, h.challenge())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, rec := h.authRequest(request); code != http.StatusOK {
		t.Fatalf("auth: %d %s", code, rec.Body.String())
	}
	if code, _, rec := h.authRequest(request); code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "challenge unknown or expired") {
		t.Fatalf("replayed challenge: %d %s", code, rec.Body.String())
	}

	stale, err := brokerapi.SignUserAuth(key, h.challenge())
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(userChallengeTTL)
	if code, _, _ := h.authRequest(stale); code != http.StatusUnauthorized {
		t.Fatalf("expired challenge: %d", code)
	}

	if code, _, _ := h.authRequest(brokerapi.UserAuthRequest{PublicKey: stale.PublicKey, Challenge: "never-issued", Signature: stale.Signature}); code != http.StatusUnauthorized {
		t.Fatalf("unknown challenge: %d", code)
	}
}

func TestUserAuthRejectsBadSignaturesWithoutBurningBudget(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{EnrollmentBudget: EnrollmentBudget{BurstLimit: 1, DailyLimit: 1}})
	key := newUserKey(t)

	// Signature under the wrong key.
	good, err := brokerapi.SignUserAuth(key, h.challenge())
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := brokerapi.SignUserAuth(newUserKey(t), good.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey.PublicKey = good.PublicKey
	if code, _, rec := h.authRequest(wrongKey); code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid signature") {
		t.Fatalf("wrong-key signature: %d %s", code, rec.Body.String())
	}

	// Tampered signature on a fresh challenge.
	tampered, _ := brokerapi.SignUserAuth(key, h.challenge())
	raw, _ := base64.StdEncoding.DecodeString(tampered.Signature)
	raw[0] ^= 0xff
	tampered.Signature = base64.StdEncoding.EncodeToString(raw)
	if code, _, _ := h.authRequest(tampered); code != http.StatusUnauthorized {
		t.Fatalf("tampered signature: %d", code)
	}

	// The budget allows exactly one new identity; the failures above must
	// not have consumed it.
	if code, _, rec := h.auth(key); code != http.StatusOK {
		t.Fatalf("legitimate auth after failures: %d %s", code, rec.Body.String())
	}
}

func TestUserAuthRejectsMalformedRequests(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{})
	key := newUserKey(t)
	good, _ := brokerapi.SignUserAuth(key, h.challenge())
	payload, _ := json.Marshal(good)

	for name, body := range map[string]string{
		"not json":      `nope`,
		"unknown field": strings.TrimSuffix(string(payload), "}") + `,"extra":1}`,
		"two objects":   string(payload) + string(payload),
		"empty":         ``,
		"bad key":       `{"public_key":"QUJD","challenge":"` + good.Challenge + `","signature":"` + good.Signature + `"}`,
		"spaced chal":   `{"public_key":"` + good.PublicKey + `","challenge":"a b","signature":"` + good.Signature + `"}`,
		"no challenge":  `{"public_key":"` + good.PublicKey + `","signature":"` + good.Signature + `"}`,
	} {
		rec := h.do(http.MethodPost, "/api/v1/users/auth", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	// None of the malformed requests consumed the challenge.
	if code, _, rec := h.authRequest(good); code != http.StatusOK {
		t.Fatalf("good request after malformed ones: %d %s", code, rec.Body.String())
	}
	rec := h.do(http.MethodPost, "/api/v1/users/auth", strings.Repeat(" ", maxUserAuthBodyBytes+1), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", rec.Code)
	}
}

func TestUserEnrollmentBudgetPerSource(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{EnrollmentBudget: EnrollmentBudget{BurstLimit: 2, BurstWindow: time.Hour, DailyLimit: 3, DailyWindow: 24 * time.Hour}})

	keys := []*ecdsa.PrivateKey{newUserKey(t), newUserKey(t), newUserKey(t), newUserKey(t)}
	for i := 0; i < 2; i++ {
		if code, _, rec := h.auth(keys[i]); code != http.StatusOK {
			t.Fatalf("new identity %d: %d %s", i+1, code, rec.Body.String())
		}
	}
	code, _, rec := h.auth(keys[2])
	if code != http.StatusTooManyRequests {
		t.Fatalf("third identity within the burst window: %d %s", code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(3600) {
		t.Fatalf("Retry-After = %q, want the burst window remainder 3600", got)
	}
	// Existing identities authenticate regardless of the budget.
	if code, _, rec := h.auth(keys[0]); code != http.StatusOK {
		t.Fatalf("existing identity while budget exhausted: %d %s", code, rec.Body.String())
	}
	// Another source has its own budget. IPv6 sources are bucketed by /64:
	// two addresses in one /64 share.
	h.source = "[2001:db8:1:2::1]:5000"
	if code, _, _ := h.auth(keys[2]); code != http.StatusOK {
		t.Fatalf("other source: %d", code)
	}
	h.source = "[2001:db8:1:2:ffff::9]:5000"
	if code, _, _ := h.auth(keys[3]); code != http.StatusOK {
		t.Fatalf("same /64 second identity: %d", code)
	}
	if code, _, _ := h.auth(newUserKey(t)); code != http.StatusTooManyRequests {
		t.Fatalf("same /64 third identity: %d, want 429", code)
	}

	// Back on the first source: the burst window resets after an hour but
	// the daily window still holds one more, then refuses.
	h.source = "203.0.113.10:4000"
	h.now = h.now.Add(time.Hour)
	if code, _, _ := h.auth(newUserKey(t)); code != http.StatusOK {
		t.Fatalf("after burst reset: %d", code)
	}
	code, _, rec = h.auth(newUserKey(t))
	if code != http.StatusTooManyRequests {
		t.Fatalf("daily limit: %d", code)
	}
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(23*3600) {
		t.Fatalf("daily Retry-After = %q, want 82800", got)
	}
	// Refusals never changed the counters: still exactly one slot after the
	// daily window ends.
	h.now = h.now.Add(23 * time.Hour)
	if code, _, _ := h.auth(newUserKey(t)); code != http.StatusOK {
		t.Fatalf("after daily reset: %d", code)
	}
}

func TestUserDisableRevokesTokensAndAdminViews(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{APIToken: "api-token"})
	key := newUserKey(t)
	code, auth, _ := h.auth(key)
	if code != http.StatusOK {
		t.Fatalf("auth: %d", code)
	}
	id := auth.UserID

	rec := h.do(http.MethodGet, "/admin/api/users/"+id, "", map[string]string{"Authorization": "Bearer api-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin get: %d %s", rec.Code, rec.Body.String())
	}
	var view userView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.UserID != id || view.Status != brokerapi.UserStatusEnabled || view.DisabledAt != nil {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(rec.Body.String(), "public_key") || strings.Contains(rec.Body.String(), "BA") && strings.Contains(rec.Body.String(), "=") {
		t.Fatalf("admin view leaks key material: %s", rec.Body.String())
	}

	rec = h.setStatus(id, `{"status":"disabled","reason":"abuse report #12"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != brokerapi.UserStatusDisabled || view.DisabledAt == nil || view.DisabledReason != "abuse report #12" {
		t.Fatalf("disabled view = %+v", view)
	}

	code, denied, rec := h.auth(key)
	if code != http.StatusForbidden {
		t.Fatalf("disabled user auth: %d %s", code, rec.Body.String())
	}
	if denied.UserID != id || denied.Status != brokerapi.UserStatusDisabled || denied.AccessToken != "" {
		t.Fatalf("disabled auth response %s", denied)
	}

	rec = h.setStatus(id, `{"status":"enabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	view = userView{}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != brokerapi.UserStatusEnabled || view.DisabledAt != nil || view.DisabledReason != "" {
		t.Fatalf("re-enabled view = %+v", view)
	}
	if code, _, _ := h.auth(key); code != http.StatusOK {
		t.Fatalf("re-enabled auth: %d", code)
	}

	// Admin surface hygiene.
	for name, tc := range map[string]struct {
		method, path, body, token string
		want                      int
	}{
		"no token get":     {http.MethodGet, "/admin/api/users/" + id, "", "", http.StatusUnauthorized},
		"no token put":     {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":"disabled"}`, "", http.StatusUnauthorized},
		"wrong method":     {http.MethodDelete, "/admin/api/users/" + id, "", "api-token", http.StatusMethodNotAllowed},
		"get on status":    {http.MethodGet, "/admin/api/users/" + id + "/status", "", "api-token", http.StatusMethodNotAllowed},
		"unknown user":     {http.MethodGet, "/admin/api/users/user_" + strings.Repeat("0", 32), "", "api-token", http.StatusNotFound},
		"malformed id":     {http.MethodGet, "/admin/api/users/nope", "", "api-token", http.StatusNotFound},
		"unknown put":      {http.MethodPut, "/admin/api/users/user_" + strings.Repeat("0", 32) + "/status", `{"status":"disabled"}`, "api-token", http.StatusNotFound},
		"bad status":       {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":"banned"}`, "api-token", http.StatusBadRequest},
		"unknown field":    {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":"disabled","note":"x"}`, "api-token", http.StatusBadRequest},
		"long reason":      {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":"disabled","reason":"` + strings.Repeat("r", maxUserStatusReasonBytes+1) + `"}`, "api-token", http.StatusBadRequest},
		"control reason":   {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":"disabled","reason":"a\u0000b"}`, "api-token", http.StatusBadRequest},
		"case-insensitive": {http.MethodPut, "/admin/api/users/" + id + "/status", `{"status":" Enabled "}`, "api-token", http.StatusOK},
	} {
		headers := map[string]string{}
		if tc.token != "" {
			headers["Authorization"] = "Bearer " + tc.token
		}
		rec := h.do(tc.method, tc.path, tc.body, headers)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestUserRoutesAbsentWithoutTokenKey(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), APIToken: "api-token"})
	for _, path := range []string{"/api/v1/users/challenge", "/api/v1/users/auth"} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without a token key: %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/users/user_"+strings.Repeat("0", 32), nil)
	req.Header.Set("Authorization", "Bearer api-token")
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin user route without a token key: %d", rec.Code)
	}
}

func TestUserRoutesRegisteredThroughNewServer(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), UserTokenKey: testUserTokenKey(), APIToken: "api-token"})
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/users/challenge", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("challenge through NewServer: %d %s", rec.Code, rec.Body.String())
	}
	var challenge brokerapi.UserChallenge
	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	request, err := brokerapi.SignUserAuth(newUserKey(t), challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(request)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/users/auth", bytes.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("auth through NewServer: %d %s", rec.Code, rec.Body.String())
	}
	var auth brokerapi.UserAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &auth)
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+auth.UserID+"/status", strings.NewReader(`{"status":"disabled","reason":"test"}`))
	req.Header.Set("Authorization", "Bearer api-token")
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status through NewServer: %d %s", rec.Code, rec.Body.String())
	}
	// The heartbeat catch-all pattern must not swallow the user routes.
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+auth.UserID+"/heartbeat", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("heartbeat for a non-relay id: %d", rec.Code)
	}
}

func TestNewServerPanicsWhenStoreLacksAccessSupport(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer accepted a token key with a store that cannot hold users")
		}
	}()
	NewServer(relayOnlyStore{NewStore()}, Config{SigningSeed: testSigningSeed(), UserTokenKey: testUserTokenKey()})
}

func TestAccessTokenCodec(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	codec, err := newAccessTokenCodec(testUserTokenKey(), 0)
	if err != nil {
		t.Fatal(err)
	}
	token, expiresAt, err := codec.issue("user_abc", now)
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(now.Add(defaultAccessTokenTTL)) {
		t.Fatalf("expires %s", expiresAt)
	}
	if id, ok := codec.verify(token, now); !ok || id != "user_abc" {
		t.Fatalf("verify = %q %v", id, ok)
	}
	if id, ok := codec.verify(token, expiresAt.Add(-time.Second)); !ok || id != "user_abc" {
		t.Fatalf("verify just before expiry = %q %v", id, ok)
	}
	if _, ok := codec.verify(token, expiresAt); ok {
		t.Fatal("token verified at its expiry instant")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	for _, i := range []int{0, 1, 5, len(raw) - 1} {
		flipped := append([]byte(nil), raw...)
		flipped[i] ^= 0x01
		if _, ok := codec.verify(base64.RawURLEncoding.EncodeToString(flipped), now); ok {
			t.Fatalf("token with byte %d flipped verified", i)
		}
	}
	if _, ok := codec.verify(token[:len(token)-4], now); ok {
		t.Fatal("truncated token verified")
	}
	if _, ok := codec.verify("", now); ok {
		t.Fatal("empty token verified")
	}
	otherKey, _ := newAccessTokenCodec(bytes.Repeat([]byte{0x01}, accessTokenKeyBytes), 0)
	if _, ok := otherKey.verify(token, now); ok {
		t.Fatal("token verified under another key")
	}
	if _, err := newAccessTokenCodec([]byte("short"), 0); err == nil {
		t.Fatal("accepted a short key")
	}
	if _, err := newAccessTokenCodec(testUserTokenKey(), time.Second); err == nil {
		t.Fatal("accepted a one-second TTL")
	}
	if _, err := newAccessTokenCodec(testUserTokenKey(), 30*24*time.Hour); err == nil {
		t.Fatal("accepted a 30-day TTL")
	}
}

func TestChallengeTableBoundsAndSingleUse(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	table := newChallengeTable(time.Minute, 2)
	a, _, ok := table.issue(now)
	if !ok {
		t.Fatal("issue a")
	}
	if _, _, ok := table.issue(now); !ok {
		t.Fatal("issue b")
	}
	if _, _, ok := table.issue(now); ok {
		t.Fatal("issued past the bound with live entries")
	}
	// Expired entries are swept to make room.
	if _, _, ok := table.issue(now.Add(time.Minute)); !ok {
		t.Fatal("issue after expiry sweep")
	}
	if table.consume(a, now.Add(time.Minute)) {
		t.Fatal("consumed an expired (swept) challenge")
	}
	c, _, _ := table.issue(now)
	if !table.consume(c, now.Add(30*time.Second)) {
		t.Fatal("consume c")
	}
	if table.consume(c, now) {
		t.Fatal("consumed c twice")
	}
	if table.consume("never", now) {
		t.Fatal("consumed an unknown challenge")
	}
}

func TestWellFormedUserID(t *testing.T) {
	for id, want := range map[string]bool{
		"user_" + strings.Repeat("0", 32):  true,
		"user_" + strings.Repeat("a", 32):  true,
		"user_" + strings.Repeat("A", 32):  false,
		"user_" + strings.Repeat("0", 31):  false,
		"relay_" + strings.Repeat("0", 32): false,
		"":                                 false,
		"user_":                            false,
	} {
		if got := wellFormedUserID(id); got != want {
			t.Errorf("wellFormedUserID(%q) = %v, want %v", id, got, want)
		}
	}
	id, err := newUserID()
	if err != nil || !wellFormedUserID(id) {
		t.Fatalf("newUserID() = %q, %v", id, err)
	}
}

func TestSourceBucketKey(t *testing.T) {
	for source, want := range map[string]string{
		"203.0.113.10":              "203.0.113.10",
		"::ffff:203.0.113.10":       "203.0.113.10",
		"2001:db8:1:2:3:4:5:6":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::1": "2001:db8:1:2::/64",
		"garbage":                   "garbage",
	} {
		if got := sourceBucketKey(source); got != want {
			t.Errorf("sourceBucketKey(%q) = %q, want %q", source, got, want)
		}
	}
}

// AccessStore conformance: the same scenarios against every implementation.
// The Postgres run is gated on OPENRUNG_TEST_POSTGRES_URL like the other
// Postgres store tests.
func TestAccessStoreConformance(t *testing.T) {
	t.Run("memory", func(t *testing.T) { accessStoreConformance(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { accessStoreConformance(t, newTestPostgresStore(t)) })
}

func accessStoreConformance(t *testing.T, store AccessStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	windows := EnrollmentBudget{BurstLimit: 2, BurstWindow: time.Hour, DailyLimit: 3, DailyWindow: 24 * time.Hour}.windows()
	keyOf := func(t *testing.T) string {
		t.Helper()
		canonical, err := brokerapi.CanonicalUserPublicKey(&newUserKey(t).PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return canonical
	}

	keyA := keyOf(t)
	userA, created, err := store.AuthenticateUser(ctx, keyA, "src-1", windows, nil, now)
	if err != nil || !created {
		t.Fatalf("first auth: %+v %v %v", userA, created, err)
	}
	if userA.PublicKey != keyA || userA.Status != brokerapi.UserStatusEnabled || !userA.CreatedAt.Equal(now) || !userA.LastAuthAt.Equal(now) || !userA.DisabledAt.IsZero() {
		t.Fatalf("new user = %+v", userA)
	}
	again, created, err := store.AuthenticateUser(ctx, keyA, "src-2", windows, nil, now.Add(time.Minute))
	if err != nil || created || again.ID != userA.ID || !again.LastAuthAt.Equal(now.Add(time.Minute)) || !again.CreatedAt.Equal(now) {
		t.Fatalf("repeat auth: %+v %v %v", again, created, err)
	}

	// Budget: src-1 has used 1 of burst 2; one more new identity passes, the
	// third is refused with the burst remainder, and the refusal is not
	// charged (the daily window must still hold exactly 3 after the burst
	// window resets).
	if _, created, err := store.AuthenticateUser(ctx, keyOf(t), "src-1", windows, nil, now.Add(2*time.Minute)); err != nil || !created {
		t.Fatalf("second identity: %v %v", created, err)
	}
	_, _, err = store.AuthenticateUser(ctx, keyOf(t), "src-1", windows, nil, now.Add(3*time.Minute))
	var limited *EnrollmentLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("third identity: %v, want EnrollmentLimitedError", err)
	}
	if limited.RetryAfter != time.Hour-3*time.Minute {
		t.Fatalf("burst retry after = %s, want 57m", limited.RetryAfter)
	}
	if _, created, err := store.AuthenticateUser(ctx, keyOf(t), "src-1", windows, nil, now.Add(time.Hour)); err != nil || !created {
		t.Fatalf("after burst reset: %v %v", created, err)
	}
	_, _, err = store.AuthenticateUser(ctx, keyOf(t), "src-1", windows, nil, now.Add(time.Hour+time.Minute))
	if !errors.As(err, &limited) || limited.RetryAfter != 23*time.Hour-time.Minute {
		t.Fatalf("daily limit: %v", err)
	}
	// src-2 charged nothing for the repeat auth above.
	if _, created, err := store.AuthenticateUser(ctx, keyOf(t), "src-2", windows, nil, now); err != nil || !created {
		t.Fatalf("src-2 first identity: %v %v", created, err)
	}
	// No windows means no budget at all.
	if _, created, err := store.AuthenticateUser(ctx, keyOf(t), "src-1", nil, nil, now.Add(2*time.Hour)); err != nil || !created {
		t.Fatalf("budget disabled: %v %v", created, err)
	}

	// Lookups and status.
	got, err := store.UserByID(ctx, userA.ID)
	if err != nil || got.ID != userA.ID {
		t.Fatalf("UserByID: %+v %v", got, err)
	}
	if _, err := store.UserByID(ctx, "user_"+strings.Repeat("0", 32)); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown UserByID: %v", err)
	}
	disabledAt := now.Add(3 * time.Hour)
	disabled, _, err := store.SetUserStatus(ctx, userA.ID, brokerapi.UserStatusDisabled, "resale", disabledAt)
	if err != nil || disabled.Status != brokerapi.UserStatusDisabled || !disabled.DisabledAt.Equal(disabledAt) || disabled.DisabledReason != "resale" {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
	stillDisabled, created, err := store.AuthenticateUser(ctx, keyA, "src-1", windows, nil, disabledAt.Add(time.Minute))
	if err != nil || created || stillDisabled.Status != brokerapi.UserStatusDisabled {
		t.Fatalf("auth while disabled: %+v %v %v", stillDisabled, created, err)
	}
	enabled, _, err := store.SetUserStatus(ctx, userA.ID, brokerapi.UserStatusEnabled, "ignored", disabledAt.Add(time.Hour))
	if err != nil || enabled.Status != brokerapi.UserStatusEnabled || !enabled.DisabledAt.IsZero() || enabled.DisabledReason != "" {
		t.Fatalf("enable: %+v %v", enabled, err)
	}
	if _, _, err := store.SetUserStatus(ctx, userA.ID, "banned", "", now); !errors.Is(err, ErrInvalidUserStatus) {
		t.Fatalf("invalid status: %v", err)
	}
	if _, _, err := store.SetUserStatus(ctx, "user_"+strings.Repeat("0", 32), brokerapi.UserStatusDisabled, "", now); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown SetUserStatus: %v", err)
	}

	// Concurrent first authentications of one key yield one user, and with
	// the budget enabled exactly one of them pays: the source still has one
	// of its two burst slots afterwards, and only one.
	keyC := keyOf(t)
	later := now.Add(48 * time.Hour)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	createds := make([]bool, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user, created, err := store.AuthenticateUser(ctx, keyC, "src-race", windows, nil, later)
			ids[i], createds[i], errs[i] = user.ID, created, err
		}(i)
	}
	wg.Wait()
	creators := 0
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("concurrent auth %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("concurrent auths minted distinct users: %q vs %q", ids[i], ids[0])
		}
		if createds[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d concurrent auths reported created=true, want exactly 1", creators)
	}
	if _, created, err := store.AuthenticateUser(ctx, keyOf(t), "src-race", windows, nil, later.Add(time.Minute)); err != nil || !created {
		t.Fatalf("second burst slot after the race: %v %v (the race charged more than one slot)", created, err)
	}
	if _, _, err := store.AuthenticateUser(ctx, keyOf(t), "src-race", windows, nil, later.Add(2*time.Minute)); !errors.As(err, &limited) {
		t.Fatalf("third identity after the race: %v, want EnrollmentLimitedError", err)
	}

	// A refusal rolls back the would-be user as well: the refused key is
	// still unknown and can enroll elsewhere.
	refusedKey := keyOf(t)
	if _, _, err := store.AuthenticateUser(ctx, refusedKey, "src-race", windows, nil, later.Add(3*time.Minute)); !errors.As(err, &limited) {
		t.Fatalf("refused key: %v", err)
	}
	if _, created, err := store.AuthenticateUser(ctx, refusedKey, "src-fresh", windows, nil, later.Add(4*time.Minute)); err != nil || !created {
		t.Fatalf("refused key enrolling from another source: %v %v (refusal left a user row behind)", created, err)
	}

	// refuseNew refuses only a new key, before any charge, storing nothing;
	// an existing key authenticates through it.
	refusal := errors.New("refused by source")
	newKey := keyOf(t)
	if _, _, err := store.AuthenticateUser(ctx, newKey, "src-refuse", windows, refusal, later.Add(5*time.Minute)); !errors.Is(err, refusal) {
		t.Fatalf("new key with refuseNew: %v, want the refusal", err)
	}
	if user, created, err := store.AuthenticateUser(ctx, keyA, "src-refuse", windows, refusal, later.Add(5*time.Minute)); err != nil || created || user.ID != userA.ID {
		t.Fatalf("existing key with refuseNew: %+v %v %v", user, created, err)
	}
	// Neither left a charge on src-refuse nor a row for newKey: the source's
	// whole burst budget (2) is still available, the first slot to newKey.
	for i, key := range []string{newKey, keyOf(t)} {
		if _, created, err := store.AuthenticateUser(ctx, key, "src-refuse", windows, nil, later.Add(6*time.Minute)); err != nil || !created {
			t.Fatalf("enrollment %d after refusals: %v %v", i+1, created, err)
		}
	}
}

func TestStorePruneDropsEndedEnrollmentWindows(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	windows := EnrollmentBudget{BurstLimit: 1, BurstWindow: time.Hour, DailyLimit: 1, DailyWindow: 24 * time.Hour}.windows()
	canonical, _ := brokerapi.CanonicalUserPublicKey(&newUserKey(t).PublicKey)
	if _, _, err := store.AuthenticateUser(context.Background(), canonical, "src", windows, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(now.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.enrollment["src"]["burst"]; ok {
		t.Fatal("burst counter survived Prune after its window ended")
	}
	if _, ok := store.enrollment["src"]["day"]; !ok {
		t.Fatal("day counter pruned while its window was live")
	}
	if _, err := store.Prune(now.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.enrollment["src"]; ok {
		t.Fatal("source entry survived after every window ended")
	}
}

// relayOnlyStore exposes only the RelayStore surface of a store: embedding
// the interface (not *Store) promotes no AccessStore methods.
type relayOnlyStore struct {
	RelayStore
}
