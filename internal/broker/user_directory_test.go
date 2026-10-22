// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"openrung/internal/relay"
)

// User directory conformance: the same scenario against both stores. The
// Postgres run is gated on OPENRUNG_TEST_POSTGRES_URL like the other
// Postgres store tests.
func TestUserDirectoryConformance(t *testing.T) {
	t.Run("memory", func(t *testing.T) { userDirectoryConformance(t, NewStore()) })
	t.Run("postgres", func(t *testing.T) { userDirectoryConformance(t, newTestPostgresStore(t)) })
}

type directoryStore interface {
	AccessStore
	userDirectory
}

func userDirectoryConformance(t *testing.T, store directoryStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	now := at(26 * time.Hour) // the counters' window starts at base+2h
	r1, r2, r3 := "relay_"+strings.Repeat("1", 32), "relay_"+strings.Repeat("2", 32), "relay_"+strings.Repeat("3", 32)

	keys := map[string]string{} // user ID → canonical key, for re-authentication
	enroll := func(created time.Time) string {
		t.Helper()
		canonical, err := brokerapi.CanonicalUserPublicKey(&newUserKey(t).PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		user, _, err := store.AuthenticateUser(ctx, canonical, "src", nil, nil, created)
		if err != nil {
			t.Fatal(err)
		}
		keys[user.ID] = canonical
		return user.ID
	}
	reauth := func(id string, when time.Time) {
		t.Helper()
		if _, _, err := store.AuthenticateUser(ctx, keys[id], "src", nil, nil, when); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(id, relayID string, when time.Time, ttl time.Duration) Lease {
		t.Helper()
		result, err := store.IssueLeases(ctx, LeaseIssueRequest{UserID: id, RelayIDs: []string{relayID}, TTL: ttl}, when)
		if err != nil || len(result.Leases) != 1 {
			t.Fatalf("issue %s on %s: %+v %v", id, relayID, result, err)
		}
		return result.Leases[0]
	}

	// A: two old leases outside the window, one live lease inside it.
	userA := enroll(at(0))
	issue(userA, r1, at(time.Hour), time.Hour)
	issue(userA, r2, at(time.Hour), time.Hour)
	issue(userA, r3, at(25*time.Hour), 2*time.Hour)
	reauth(userA, at(25*time.Hour))
	// B: three leases issued in the window over two relays; one still live.
	userB := enroll(at(time.Hour))
	issue(userB, r1, at(20*time.Hour), time.Hour)
	olderB := issue(userB, r2, at(24*time.Hour), time.Hour)
	latestB := issue(userB, r1, at(25*time.Hour+30*time.Minute), 2*time.Hour)
	reauth(userB, at(25*time.Hour+30*time.Minute))
	// C: a lease created before the window but renewed inside it — a relay
	// in the window, live, yet not issued in the window.
	userC := enroll(at(2 * time.Hour))
	issue(userC, r2, at(time.Hour), 24*time.Hour)
	if renewed := issue(userC, r2, at(5*time.Hour), 24*time.Hour); !renewed.CreatedAt.Equal(at(time.Hour)) {
		t.Fatalf("renewal minted a new lease: %+v", renewed)
	}
	reauth(userC, at(5*time.Hour))
	// D: disabled after one lease, which the disable revoked.
	userD := enroll(at(3 * time.Hour))
	issue(userD, r1, at(22*time.Hour), 2*time.Hour)
	reauth(userD, at(22*time.Hour))
	if _, _, err := store.SetUserStatus(ctx, userD, brokerapi.UserStatusDisabled, "abuse report", at(23*time.Hour)); err != nil {
		t.Fatal(err)
	}

	list := func(q UserListQuery) UserListPage {
		t.Helper()
		if q.Sort == "" {
			q.Sort = UserSortLastAuth
		}
		if q.Limit == 0 {
			q.Limit = userListDefaultLimit
		}
		page, err := store.ListUsers(ctx, q, now)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		return page
	}
	ids := func(page UserListPage) []string {
		out := []string{}
		for _, entry := range page.Users {
			out = append(out, entry.ID)
		}
		return out
	}
	expectIDs := func(name string, page UserListPage, total int, want ...string) {
		t.Helper()
		if got := ids(page); page.Total != total || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: total %d ids %v, want total %d ids %v", name, page.Total, got, total, want)
		}
	}

	all := list(UserListQuery{})
	expectIDs("last_auth", all, 4, userB, userA, userD, userC)
	expectIDs("enrolled", list(UserListQuery{Sort: UserSortEnrolled}), 4, userD, userC, userB, userA)
	// A and D tie on one issued lease; last_auth breaks it.
	expectIDs("leases_24h", list(UserListQuery{Sort: UserSortLeases24h}), 4, userB, userA, userD, userC)
	expectIDs("disabled", list(UserListQuery{Status: brokerapi.UserStatusDisabled}), 1, userD)
	expectIDs("enabled", list(UserListQuery{Status: brokerapi.UserStatusEnabled}), 3, userB, userA, userC)
	expectIDs("page", list(UserListQuery{Offset: 1, Limit: 2}), 4, userA, userD)
	expectIDs("past the end", list(UserListQuery{Offset: 10, Limit: 2}), 4)
	expectIDs("one user", list(UserListQuery{UserID: userC}), 1, userC)
	expectIDs("one user, other status", list(UserListQuery{UserID: userC, Status: brokerapi.UserStatusDisabled}), 0)

	want := map[string][3]int{ // live, issued 24h, relays 24h
		userA: {1, 1, 1},
		userB: {1, 3, 2},
		userC: {1, 0, 1},
		userD: {0, 1, 1},
	}
	for _, entry := range all.Users {
		if got := [3]int{entry.LiveLeases, entry.LeasesIssued24h, entry.Relays24h}; got != want[entry.ID] {
			t.Errorf("%s counters = %v, want %v", entry.ID, got, want[entry.ID])
		}
	}
	d := all.Users[2]
	if d.ID != userD || d.Status != brokerapi.UserStatusDisabled || d.DisabledReason != "abuse report" || !d.DisabledAt.Equal(at(23*time.Hour)) || !d.CreatedAt.Equal(at(3*time.Hour)) || !d.LastAuthAt.Equal(at(22*time.Hour)) {
		t.Errorf("disabled row = %+v", d)
	}
	if a := all.Users[1]; !a.DisabledAt.IsZero() || a.DisabledReason != "" {
		t.Errorf("enabled row carries disable fields: %+v", a)
	}

	// Lease history: most recent first, bounded, with the retained total.
	leases, total, err := store.UserLeases(ctx, userB, 2)
	if err != nil || total != 3 || len(leases) != 2 || leases[0].ID != latestB.ID || leases[1].ID != olderB.ID {
		t.Fatalf("user leases = %+v total %d err %v", leases, total, err)
	}
	if leases, total, err := store.UserLeases(ctx, "user_"+strings.Repeat("0", 32), 5); err != nil || total != 0 || len(leases) != 0 {
		t.Fatalf("unknown user leases = %+v %d %v", leases, total, err)
	}

	// Search: a lease label or bare lease ID resolves to the owner.
	for _, ref := range []string{latestB.Email(), latestB.ID, " " + strings.ToUpper(latestB.Email()) + " "} {
		userID, leaseID, err := resolveUserRef(ctx, store, ref)
		if err != nil || userID != userB || leaseID != latestB.ID {
			t.Errorf("resolve %q = %q %q %v, want %s via %s", ref, userID, leaseID, err, userB, latestB.ID)
		}
	}
	if userID, leaseID, err := resolveUserRef(ctx, store, userC); err != nil || userID != userC || leaseID != "" {
		t.Errorf("resolve user id = %q %q %v", userID, leaseID, err)
	}
	for _, ref := range []string{"", "nope", "lease-xyz", "lease-" + strings.Repeat("0", 32), "relay_" + strings.Repeat("1", 32)} {
		if _, _, err := resolveUserRef(ctx, store, ref); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("resolve %q err = %v, want ErrUserNotFound", ref, err)
		}
	}

	// Enrollment ties break on the user ID alone, on both stores: a later
	// re-authentication of the lower ID must not move it ahead.
	tiedA, tiedB := enroll(at(4*time.Hour)), enroll(at(4*time.Hour))
	higher, lower := tiedA, tiedB
	if lower > higher {
		higher, lower = lower, higher
	}
	reauth(lower, at(25*time.Hour+45*time.Minute))
	expectIDs("enrolled tie", list(UserListQuery{Sort: UserSortEnrolled, Limit: 2}), 6, higher, lower)
	expectIDs("last_auth after the tie", list(UserListQuery{Limit: 2}), 6, lower, userB)
}

func TestParseUserListQuery(t *testing.T) {
	for raw, wantProblem := range map[string]bool{
		"":                                 false,
		"status=Disabled":                  false,
		"sort=leases_24h":                  false,
		"sort=enrolled&limit=100&offset=5": false,
		"status=banned":                    true,
		"sort=public_key":                  true,
		"limit=0":                          true,
		"limit=101":                        true,
		"offset=-1":                        true,
		"offset=x":                         true,
	} {
		values, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		q, problem := parseUserListQuery(values)
		if (problem != "") != wantProblem {
			t.Errorf("%q: problem %q, want problem %v", raw, problem, wantProblem)
		}
		if raw == "" && (q.Sort != UserSortLastAuth || q.Limit != userListDefaultLimit || q.Offset != 0 || q.Status != "") {
			t.Errorf("defaults = %+v", q)
		}
		if raw == "status=Disabled" && q.Status != brokerapi.UserStatusDisabled {
			t.Errorf("status not normalized: %+v", q)
		}
	}
}

// usersPageHarness is a full NewServer with client users, the operational API
// and the dashboard, logged in.
type usersPageHarness struct {
	t      *testing.T
	store  *Store
	server http.Handler
	cookie *http.Cookie
}

func newUsersPageHarness(t *testing.T) *usersPageHarness {
	t.Helper()
	store := NewStore()
	telemetry := &dashboardTelemetryStore{}
	server := NewServer(store, Config{
		SigningSeed: testSigningSeed(), TelemetrySink: telemetry, TelemetryReader: telemetry,
		DashboardToken: "secret-token", UserTokenKey: testUserTokenKey(), APIToken: "api-token",
	})
	login := postLogin(server, "secret-token")
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", login.Code)
	}
	return &usersPageHarness{t: t, store: store, server: server, cookie: login.Result().Cookies()[0]}
}

func (h *usersPageHarness) do(method, target, body string, headers map[string]string, withCookie bool) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Host = "broker.example"
	if withCookie {
		request.AddCookie(h.cookie)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	h.server.ServeHTTP(response, request)
	return response
}

func (h *usersPageHarness) enroll() string {
	h.t.Helper()
	canonical, err := brokerapi.CanonicalUserPublicKey(&newUserKey(h.t).PublicKey)
	if err != nil {
		h.t.Fatal(err)
	}
	user, _, err := h.store.AuthenticateUser(context.Background(), canonical, "src", nil, nil, time.Now().UTC())
	if err != nil {
		h.t.Fatal(err)
	}
	return user.ID
}

var sameOriginJSON = map[string]string{"Content-Type": "application/json", "Origin": "https://broker.example"}

func TestDashboardUsersRequireSession(t *testing.T) {
	h := newUsersPageHarness(t)
	id := h.enroll()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/admin/api/dashboard/users", ""},
		{http.MethodGet, "/admin/api/dashboard/users/" + id, ""},
		{http.MethodPut, "/admin/api/dashboard/users/" + id + "/status", `{"status":"disabled","reason":"x"}`},
	} {
		// The operational token is not a dashboard credential.
		response := h.do(tc.method, tc.path, tc.body, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer api-token"}, false)
		if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s without a session = %d (%q), want 401 no-store", tc.method, tc.path, response.Code, response.Header().Get("Cache-Control"))
		}
	}
	page := h.do(http.MethodGet, "/admin/dashboard/users", "", nil, false)
	if page.Code != http.StatusSeeOther || page.Header().Get("Location") != "/admin/dashboard/login" {
		t.Errorf("page without a session = %d → %q", page.Code, page.Header().Get("Location"))
	}
	if user, _ := h.store.UserByID(context.Background(), id); user.Status != brokerapi.UserStatusEnabled {
		t.Fatalf("unauthenticated PUT changed the user: %+v", user)
	}
}

func TestDashboardUsersListAndDetail(t *testing.T) {
	h := newUsersPageHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	registered, err := h.store.Register(relay.RegisterRequest{
		Label: "tokyo-1", NodeClass: relay.NodeClassFoundation, PublicHost: "203.0.113.5", PublicPort: 443, MaxSessions: 10, MaxMbps: 10,
	}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	quiet := h.enroll()
	busy := h.enroll()
	result, err := h.store.IssueLeases(ctx, LeaseIssueRequest{UserID: busy, RelayIDs: []string{registered.ID, "relay_" + strings.Repeat("9", 32)}, TTL: time.Hour}, now)
	if err != nil || len(result.Leases) != 2 {
		t.Fatalf("issue: %+v %v", result, err)
	}

	list := h.do(http.MethodGet, "/admin/api/dashboard/users?sort=leases_24h&limit=1", "", nil, true)
	if list.Code != http.StatusOK || list.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	var page userListResponse
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Limit != 1 || page.Sort != UserSortLeases24h || len(page.Users) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if row := page.Users[0]; row.UserID != busy || row.LiveLeases != 2 || row.LeasesIssued24h != 2 || row.Relays24h != 2 || row.Status != brokerapi.UserStatusEnabled {
		t.Errorf("row = %+v", row)
	}
	assertNoPrivateFields(t, "list", list.Body.String(), h.store)
	for _, bad := range []string{"sort=nope", "status=banned", "limit=500", "offset=-3"} {
		if response := h.do(http.MethodGet, "/admin/api/dashboard/users?"+bad, "", nil, true); response.Code != http.StatusBadRequest {
			t.Errorf("list ?%s = %d, want 400", bad, response.Code)
		}
	}

	// Detail by user ID and by the lease label a relay logs.
	var target Lease
	for _, lease := range result.Leases {
		if lease.RelayID == registered.ID {
			target = lease
		}
	}
	for _, ref := range []string{busy, target.Email(), target.ID} {
		response := h.do(http.MethodGet, "/admin/api/dashboard/users/"+ref, "", nil, true)
		if response.Code != http.StatusOK {
			t.Fatalf("detail %s = %d %s", ref, response.Code, response.Body.String())
		}
		var detail userDetailResponse
		if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.User.UserID != busy || detail.LeasesTotal != 2 || len(detail.Leases) != 2 || detail.LeaseLimit != userLeaseHistoryLimit || detail.LeaseRetentionDays != 7 {
			t.Errorf("detail %s = %+v", ref, detail)
		}
		if wantMatch := ref != busy; (detail.MatchedLease == target.ID) != wantMatch {
			t.Errorf("detail %s matched lease = %q", ref, detail.MatchedLease)
		}
		for _, lease := range detail.Leases {
			wantLabel := ""
			if lease.RelayID == registered.ID {
				wantLabel = "tokyo-1"
			}
			if lease.RelayLabel != wantLabel || lease.Status != "live" || lease.Label != relay.LeaseEmailPrefix+lease.LeaseID {
				t.Errorf("detail lease = %+v", lease)
			}
		}
		assertNoPrivateFields(t, "detail", response.Body.String(), h.store)
	}
	if response := h.do(http.MethodGet, "/admin/api/dashboard/users/"+quiet, "", nil, true); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"leases":[]`) {
		t.Errorf("detail without leases = %d %s", response.Code, response.Body.String())
	}
	for _, ref := range []string{"user_" + strings.Repeat("0", 32), "lease-" + strings.Repeat("0", 32), "nope", "user_XYZ"} {
		if response := h.do(http.MethodGet, "/admin/api/dashboard/users/"+ref, "", nil, true); response.Code != http.StatusNotFound {
			t.Errorf("detail %s = %d, want 404", ref, response.Code)
		}
	}

	// The page renders behind the session with the shared shell.
	pageResponse := h.do(http.MethodGet, "/admin/dashboard/users", "", nil, true)
	if pageResponse.Code != http.StatusOK || pageResponse.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("page = %d", pageResponse.Code)
	}
	body := pageResponse.Body.String()
	for _, needle := range []string{"<h1>Users</h1>", "/admin/api/dashboard/users", "does <b>not</b> terminate", "Reason (required)", "confirmDisable", "confirmEnable", "retained for", "lease label"} {
		if !strings.Contains(body, needle) {
			t.Errorf("users page lacks %q", needle)
		}
	}
}

// assertNoPrivateFields fails when a response carries key material or any
// field outside the directory's columns.
func assertNoPrivateFields(t *testing.T, name, body string, store *Store) {
	t.Helper()
	for _, field := range []string{"public_key", "credential", "client_id", "source_ip", "app_version"} {
		if strings.Contains(body, field) {
			t.Errorf("%s response carries %q: %s", name, field, body)
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, user := range store.users {
		if strings.Contains(body, user.PublicKey) {
			t.Errorf("%s response carries a public key", name)
		}
	}
	for _, lease := range store.leases {
		if strings.Contains(body, lease.Credential) {
			t.Errorf("%s response carries a lease credential", name)
		}
	}
}

// The status change is one implementation behind two doors: the operational
// token route and the dashboard route, distinguished only by the log line.
func TestUserStatusThroughBothDoors(t *testing.T) {
	h := newUsersPageHarness(t)
	id := h.enroll()
	path := "/admin/api/dashboard/users/" + id + "/status"
	disable := `{"status":"disabled","reason":"abuse report"}`

	// Dashboard guards: cross-origin and non-JSON are refused before anything.
	for name, headers := range map[string]map[string]string{
		"cross-site fetch metadata": {"Content-Type": "application/json", "Origin": "https://broker.example", "Sec-Fetch-Site": "cross-site"},
		"foreign origin":            {"Content-Type": "application/json", "Origin": "https://evil.example"},
		"same-site sibling":         {"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"},
	} {
		if response := h.do(http.MethodPut, path, disable, headers, true); response.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, response.Code)
		}
	}
	if response := h.do(http.MethodPut, path, disable, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, true); response.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form content type = %d, want 415", response.Code)
	}
	if user, _ := h.store.UserByID(context.Background(), id); user.Status != brokerapi.UserStatusEnabled {
		t.Fatalf("guarded requests changed the user: %+v", user)
	}
	for _, target := range []string{"user_" + strings.Repeat("0", 32), "nope"} {
		if response := h.do(http.MethodPut, "/admin/api/dashboard/users/"+target+"/status", disable, sameOriginJSON, true); response.Code != http.StatusNotFound {
			t.Errorf("dashboard PUT %s = %d, want 404", target, response.Code)
		}
	}
	// Validation is the operational API's.
	if response := h.do(http.MethodPut, path, `{"status":"banned"}`, sameOriginJSON, true); response.Code != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", response.Code)
	}

	logs := captureLogs(t)
	response := h.do(http.MethodPut, path, disable, sameOriginJSON, true)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard disable = %d %s", response.Code, response.Body.String())
	}
	var view userView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || view.Status != brokerapi.UserStatusDisabled || view.DisabledReason != "abuse report" {
		t.Fatalf("dashboard disable view = %+v %v", view, err)
	}
	if !strings.Contains(logs.String(), `msg="user status changed"`) || !strings.Contains(logs.String(), `via=dashboard`) {
		t.Errorf("dashboard door log = %s", logs.String())
	}

	// The operational list sees the decision; the operational door undoes it.
	list := h.do(http.MethodGet, "/admin/api/users?status=disabled", "", map[string]string{"Authorization": "Bearer api-token"}, false)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), id) || !strings.Contains(list.Body.String(), `"total":1`) {
		t.Fatalf("operational list = %d %s", list.Code, list.Body.String())
	}
	logs.Reset()
	enable := h.do(http.MethodPut, "/admin/api/users/"+id+"/status", `{"status":"enabled"}`, map[string]string{"Authorization": "Bearer api-token"}, false)
	if enable.Code != http.StatusOK {
		t.Fatalf("operational enable = %d %s", enable.Code, enable.Body.String())
	}
	if !strings.Contains(logs.String(), `via="operational api"`) {
		t.Errorf("operational door log = %s", logs.String())
	}
	if user, _ := h.store.UserByID(context.Background(), id); user.Status != brokerapi.UserStatusEnabled || user.DisabledReason != "" {
		t.Fatalf("after operational enable: %+v", user)
	}
}

func TestOperationalUserListRoute(t *testing.T) {
	h := newUsersPageHarness(t)
	h.enroll()
	// Credential before method; the dashboard cookie is not the API token.
	for name, tc := range map[string]struct {
		method  string
		headers map[string]string
		cookie  bool
		want    int
	}{
		"no token":       {http.MethodGet, nil, false, http.StatusUnauthorized},
		"cookie only":    {http.MethodGet, nil, true, http.StatusUnauthorized},
		"no token, POST": {http.MethodPost, nil, false, http.StatusUnauthorized},
		"wrong method":   {http.MethodPost, map[string]string{"Authorization": "Bearer api-token"}, false, http.StatusMethodNotAllowed},
		"token":          {http.MethodGet, map[string]string{"Authorization": "Bearer api-token"}, false, http.StatusOK},
	} {
		response := h.do(tc.method, "/admin/api/users", "", tc.headers, tc.cookie)
		if response.Code != tc.want || response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d (%q), want %d no-store", name, response.Code, response.Header().Get("Cache-Control"), tc.want)
		}
	}
	if response := h.do(http.MethodGet, "/admin/api/users?sort=nope", "", map[string]string{"Authorization": "Bearer api-token"}, false); response.Code != http.StatusBadRequest {
		t.Errorf("bad sort = %d, want 400", response.Code)
	}
}

// Without client users the operational list does not exist and the
// dashboard's users API answers 503 rather than an empty directory.
func TestUsersPageWithoutClientUsers(t *testing.T) {
	telemetry := &dashboardTelemetryStore{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: telemetry, TelemetryReader: telemetry, DashboardToken: "secret-token", APIToken: "api-token"})
	request := httptest.NewRequest(http.MethodGet, "/admin/api/users", nil)
	request.Header.Set("Authorization", "Bearer api-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Errorf("operational list without users = %d, want 404", response.Code)
	}
	cookie := postLogin(server, "secret-token").Result().Cookies()[0]
	request = httptest.NewRequest(http.MethodGet, "/admin/api/dashboard/users", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("dashboard list without users = %d, want 503", response.Code)
	}
}
