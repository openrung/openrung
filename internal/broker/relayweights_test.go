package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

const testWeightRelayID = "relay_0123456789abcdef0123456789abcdef"

// operationalRequest issues one request against handler with the operational
// API token and an optional JSON-ish body.
func operationalRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeWeightResponse(t *testing.T, body []byte) rankingWeightResponse {
	t.Helper()
	var out rankingWeightResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode weight response: %v: %s", err, body)
	}
	return out
}

// Without the operational token the whole weight surface is unregistered —
// not 401, not 405, just 404 — exactly like the inventory.
func TestRelayWeightRoutesAbsentWithoutAPIToken(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), DashboardToken: "dashboard-token"})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/relays/weights"},
		{http.MethodPut, "/admin/api/relays/" + testWeightRelayID + "/weight"},
		{http.MethodDelete, "/admin/api/relays/" + testWeightRelayID + "/weight"},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"weight":0.5}`))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s %s without API token = %d, want 404", tc.method, tc.path, recorder.Code)
		}
	}
}

// Credential before method: a bad bearer sees 401 whatever method it uses,
// and only an accepted bearer learns which methods the route supports.
func TestRelayWeightRoutesRequireBearerBeforeMethod(t *testing.T) {
	server := inventoryServer(t, NewStore())
	for _, tc := range []struct {
		method, path, authorization string
		want                        int
		allow                       string
	}{
		{http.MethodGet, "/admin/api/relays/weights", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/admin/api/relays/weights", "Bearer wrong", http.StatusUnauthorized, ""},
		{http.MethodPost, "/admin/api/relays/weights", "Bearer " + testAPIToken, http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/admin/api/relays/" + testWeightRelayID + "/weight", "", http.StatusUnauthorized, ""},
		{http.MethodGet, "/admin/api/relays/" + testWeightRelayID + "/weight", "Bearer " + testAPIToken, http.StatusMethodNotAllowed, "PUT, DELETE"},
		{http.MethodPut, "/admin/api/relays/" + testWeightRelayID + "/weight", "Bearer dashboard-token", http.StatusUnauthorized, ""},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"weight":0.5}`))
		if tc.authorization != "" {
			request.Header.Set("Authorization", tc.authorization)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("%s %s (%q) = %d, want %d: %s", tc.method, tc.path, tc.authorization, recorder.Code, tc.want, recorder.Body.String())
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: Cache-Control = %q, want no-store", tc.method, tc.path, recorder.Header().Get("Cache-Control"))
		}
		if tc.allow != "" && recorder.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.path, recorder.Header().Get("Allow"), tc.allow)
		}
	}
}

func TestRelayWeightSetListDeleteRoundTrip(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)
	path := "/admin/api/relays/" + testWeightRelayID + "/weight"

	set := operationalRequest(t, server, http.MethodPut, path, `{"weight": 0.25}`)
	if set.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", set.Code, set.Body.String())
	}
	if got := decodeWeightResponse(t, set.Body.Bytes()); got.RelayID != testWeightRelayID || got.Weight != 0.25 || got.PreviousWeight != 1 {
		t.Errorf("PUT response = %+v, want weight 0.25 replacing 1", got)
	}

	list := operationalRequest(t, server, http.MethodGet, "/admin/api/relays/weights", "")
	if list.Code != http.StatusOK {
		t.Fatalf("GET weights = %d: %s", list.Code, list.Body.String())
	}
	var weights rankingWeightsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &weights); err != nil {
		t.Fatalf("decode weights: %v", err)
	}
	if weights.Count != 1 || weights.DefaultWeight != 1 || len(weights.Weights) != 1 || weights.Weights[0] != (rankingWeightEntry{RelayID: testWeightRelayID, Weight: 0.25}) {
		t.Errorf("weights = %+v, want one override at 0.25", weights)
	}

	// Drain to zero, then restore; each answer reports the value it replaced.
	drain := operationalRequest(t, server, http.MethodPut, path, `{"weight": 0}`)
	if got := decodeWeightResponse(t, drain.Body.Bytes()); drain.Code != http.StatusOK || got.Weight != 0 || got.PreviousWeight != 0.25 {
		t.Errorf("drain = %d %+v, want 200 weight 0 replacing 0.25", drain.Code, got)
	}
	restore := operationalRequest(t, server, http.MethodDelete, path, "")
	if got := decodeWeightResponse(t, restore.Body.Bytes()); restore.Code != http.StatusOK || got.Weight != 1 || got.PreviousWeight != 0 {
		t.Errorf("DELETE = %d %+v, want 200 weight 1 replacing 0", restore.Code, got)
	}
	again := operationalRequest(t, server, http.MethodDelete, path, "")
	if got := decodeWeightResponse(t, again.Body.Bytes()); again.Code != http.StatusOK || got.PreviousWeight != 1 {
		t.Errorf("idempotent DELETE = %d %+v, want 200 replacing the default", again.Code, got)
	}
	list = operationalRequest(t, server, http.MethodGet, "/admin/api/relays/weights", "")
	if err := json.Unmarshal(list.Body.Bytes(), &weights); err != nil || weights.Count != 0 || len(weights.Weights) != 0 {
		t.Errorf("weights after delete = %+v (%v), want an empty list", weights, err)
	}
	// The JSON shape keeps an array for an empty override set.
	if !strings.Contains(list.Body.String(), `"weights":[]`) {
		t.Errorf("empty weights should serialize as [], got %s", list.Body.String())
	}
}

func TestRelayWeightRejectsBadInput(t *testing.T) {
	store := NewStore()
	path := "/admin/api/relays/" + testWeightRelayID + "/weight"
	for _, tc := range []struct {
		name, path, body string
	}{
		{"weight above 1", path, `{"weight": 1.5}`},
		{"negative weight", path, `{"weight": -0.1}`},
		{"string weight", path, `{"weight": "0.5"}`},
		{"missing weight", path, `{}`},
		{"unknown field", path, `{"weight": 0.5, "relay_id": "x"}`},
		{"not JSON", path, `weight=0.5`},
		{"empty body", path, ``},
		{"trailing data", path, `{"weight": 0.5}{"weight": 0.1}`},
		{"array body", path, `[0.5]`},
		{"malformed id: uppercase", "/admin/api/relays/relay_0123456789ABCDEF0123456789ABCDEF/weight", `{"weight": 0.5}`},
		{"malformed id: short", "/admin/api/relays/relay_abc/weight", `{"weight": 0.5}`},
		{"malformed id: no prefix", "/admin/api/relays/0123456789abcdef0123456789abcdef01/weight", `{"weight": 0.5}`},
		{"malformed id: encoded slash", "/admin/api/relays/relay_0123456789abcdef%2F123456789abcdef/weight", `{"weight": 0.5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A server per case: the operational limiter's burst is smaller
			// than this table, and a 429 here would hide a validation gap.
			server := inventoryServer(t, store)
			response := operationalRequest(t, server, http.MethodPut, tc.path, tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s %q = %d, want 400: %s", tc.path, tc.body, response.Code, response.Body.String())
			}
		})
	}
	// A malformed ID is refused on DELETE too.
	if response := operationalRequest(t, inventoryServer(t, store), http.MethodDelete, "/admin/api/relays/relay_abc/weight", ""); response.Code != http.StatusBadRequest {
		t.Errorf("DELETE malformed id = %d, want 400", response.Code)
	}
	if weights, _ := store.RelayRankingWeights(t.Context()); len(weights) != 0 {
		t.Errorf("rejected requests stored weights: %v", weights)
	}
}

// The inventory decorates every descriptor with its effective weight —
// default 1 where nothing is stored — while the public directory channels
// never carry the field.
func TestRelayInventoryCarriesRankingWeight(t *testing.T) {
	store := NewStore()
	registerInventoryFleet(t, store, 2)
	relays, err := store.List(time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	weightedID := relays[0].ID
	if _, err := store.SetRelayRankingWeight(t.Context(), weightedID, 0.5); err != nil {
		t.Fatalf("set weight: %v", err)
	}
	server := inventoryServer(t, store)

	inventory := getInventory(t, server)
	if inventory.Code != http.StatusOK {
		t.Fatalf("inventory = %d: %s", inventory.Code, inventory.Body.String())
	}
	var decoded struct {
		Channel string `json:"channel"`
		Relays  []struct {
			ID            string   `json:"id"`
			RankingWeight *float64 `json:"ranking_weight"`
		} `json:"relays"`
	}
	if err := json.Unmarshal(inventory.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if decoded.Channel != relay.ChannelInventory || len(decoded.Relays) != 2 {
		t.Fatalf("inventory envelope wrong: %+v", decoded)
	}
	for _, row := range decoded.Relays {
		want := 1.0
		if row.ID == weightedID {
			want = 0.5
		}
		if row.RankingWeight == nil || *row.RankingWeight != want {
			t.Errorf("relay %s ranking_weight = %v, want %v", row.ID, row.RankingWeight, want)
		}
	}
	// The existing inventory decoder (relay.ListResponse) still reads the body.
	if got := decodeInventory(t, inventory.Body.Bytes()); got.Count != 2 || len(got.Relays) != 2 {
		t.Errorf("inventory no longer decodes as a relay list: %+v", got)
	}

	// The client directory is untouched: no ranking_weight anywhere in it.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "ranking_weight") {
		t.Errorf("public relay list = %d, must not carry ranking_weight: %s", recorder.Code, recorder.Body.String())
	}
}

// Dashboard door: the same store methods behind the session cookie, with the
// JSON content-type and same-origin guards, and the weight surfaced on rows.
func TestDashboardRelayWeightEndpoints(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	registered, err := store.Register(relay.RegisterRequest{
		Label: "tokyo-1", NodeClass: relay.NodeClassFoundation,
		PublicHost: "203.0.113.5", PublicPort: 443, MaxSessions: 100, MaxMbps: 500,
	}, now, time.Hour)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	telemetry := &dashboardTelemetryStore{}
	server := NewServer(store, Config{SigningSeed: testSigningSeed(), TelemetrySink: telemetry, TelemetryReader: telemetry, DashboardToken: "secret-token"})
	path := "/admin/api/telemetry/relays/" + registered.ID + "/weight"

	// No session: 401 like every other dashboard API route.
	anonymous := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"weight":0.5}`))
	anonymous.Header.Set("Content-Type", "application/json")
	anonymousResponse := httptest.NewRecorder()
	server.ServeHTTP(anonymousResponse, anonymous)
	if anonymousResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT = %d, want 401", anonymousResponse.Code)
	}

	login := postLogin(server, "secret-token")
	cookie := login.Result().Cookies()[0]
	do := func(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		request := httptest.NewRequest(method, target, reader)
		request.Host = "broker.example"
		request.AddCookie(cookie)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	jsonHeaders := map[string]string{"Content-Type": "application/json; charset=utf-8", "Origin": "https://broker.example"}

	// Guards: a form content type (what a cross-site form could send) is
	// refused, as is any Origin that is not this broker — on DELETE too.
	if response := do(http.MethodPut, path, `{"weight":0.5}`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); response.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form content type = %d, want 415", response.Code)
	}
	if response := do(http.MethodPut, path, `{"weight":0.5}`, map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}); response.Code != http.StatusForbidden {
		t.Errorf("cross-origin PUT = %d, want 403", response.Code)
	}
	if response := do(http.MethodDelete, path, "", map[string]string{"Content-Type": "application/json", "Origin": "null"}); response.Code != http.StatusForbidden {
		t.Errorf("opaque-origin DELETE = %d, want 403", response.Code)
	}
	if response := do(http.MethodDelete, path, "", map[string]string{"Origin": "https://broker.example"}); response.Code != http.StatusUnsupportedMediaType {
		t.Errorf("DELETE without JSON content type = %d, want 415", response.Code)
	}
	if weights, _ := store.RelayRankingWeights(t.Context()); len(weights) != 0 {
		t.Fatalf("guarded requests changed weights: %v", weights)
	}

	// Validation matches the operational API.
	if response := do(http.MethodPut, path, `{"weight":2}`, jsonHeaders); response.Code != http.StatusBadRequest {
		t.Errorf("weight 2 = %d, want 400", response.Code)
	}
	if response := do(http.MethodPut, "/admin/api/telemetry/relays/not-a-relay/weight", `{"weight":0.5}`, jsonHeaders); response.Code != http.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", response.Code)
	}

	// Same-origin JSON PUT lands, and the panel row reflects it.
	set := do(http.MethodPut, path, `{"weight":0.5}`, jsonHeaders)
	if set.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", set.Code, set.Body.String())
	}
	if got := decodeWeightResponse(t, set.Body.Bytes()); got.Weight != 0.5 || got.PreviousWeight != 1 {
		t.Errorf("PUT response = %+v", got)
	}
	panelResponse := do(http.MethodGet, "/admin/api/telemetry/relays?window=24h", "", nil)
	if panelResponse.Code != http.StatusOK {
		t.Fatalf("panel = %d: %s", panelResponse.Code, panelResponse.Body.String())
	}
	var panel relaysPanelResponse
	if err := json.Unmarshal(panelResponse.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode panel: %v", err)
	}
	if len(panel.Relays) != 1 || panel.Relays[0].RankingWeight != 0.5 {
		t.Fatalf("panel row weight = %+v, want 0.5", panel.Relays)
	}
	if !strings.Contains(panelResponse.Body.String(), `"ranking_weight":0.5`) {
		t.Errorf("panel JSON lacks ranking_weight: %s", panelResponse.Body.String())
	}

	// No Origin header at all is tolerated (older same-origin fetches).
	restore := do(http.MethodDelete, path, "", map[string]string{"Content-Type": "application/json"})
	if restore.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", restore.Code, restore.Body.String())
	}
	if got := decodeWeightResponse(t, restore.Body.Bytes()); got.Weight != 1 || got.PreviousWeight != 0.5 {
		t.Errorf("DELETE response = %+v", got)
	}
	panelResponse = do(http.MethodGet, "/admin/api/telemetry/relays?window=24h", "", nil)
	if err := json.Unmarshal(panelResponse.Body.Bytes(), &panel); err != nil || panel.Relays[0].RankingWeight != 1 {
		t.Errorf("panel row after restore = %+v (%v), want weight 1", panel.Relays, err)
	}

	// The page carries the control the endpoints serve.
	page := do(http.MethodGet, "/admin/telemetry/relays", "", nil)
	for _, needle := range []string{"<th>Weight</th>", "weight-save", "weight-reset", "/admin/api/telemetry/relays/", "ranking_weight"} {
		if !strings.Contains(page.Body.String(), needle) {
			t.Errorf("relays page lacks %q", needle)
		}
	}
}

// Offline rows (telemetry only, no live registration) carry their override
// too: it applies the moment the relay re-registers, so it must be visible.
func TestBuildRelaysPanelCarriesRankingWeights(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	descriptors := []relay.Descriptor{
		{ID: "relay-online", NodeClass: relay.NodeClassFoundation, PublicHost: "203.0.113.5", PublicPort: 443, MaxSessions: 100},
		{ID: "relay-plain", NodeClass: relay.NodeClassVolunteer, PublicHost: "203.0.113.6", PublicPort: 443, MaxSessions: 100},
	}
	stats := relayTelemetryStats{Relays: []relayStatRow{{RelayID: "relay-gone", NodeClass: relay.NodeClassVolunteer, Failures: 1, LastSeenAt: now}}}
	panel := buildRelaysPanel(descriptors, stats, map[string]float64{"relay-online": 0.5, "relay-gone": 0}, now, time.Hour)
	got := make(map[string]float64, len(panel.Relays))
	for _, row := range panel.Relays {
		got[row.RelayID] = row.RankingWeight
	}
	want := map[string]float64{"relay-online": 0.5, "relay-plain": 1, "relay-gone": 0}
	for id, weight := range want {
		if got[id] != weight {
			t.Errorf("row %s ranking_weight = %v, want %v", id, got[id], weight)
		}
	}
}

// End to end: a WSS-capable relay drained to 0 ranks last and is not pulled
// back into a short page by the WSS reservation — with limit=1 the client
// sees the top-ranked plain relay, not the drained one.
func TestDrainedWSSRelayIsNotReservedIntoThePage(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	wss := registerWSSRelayForTest(t, store, now, time.Hour)
	for i := 0; i < 2; i++ {
		req := validRegisterRequest()
		req.PublicHost = "203.0.113." + string(rune('1'+i))
		if _, err := store.Register(req, now, time.Hour); err != nil {
			t.Fatalf("register plain relay %d: %v", i, err)
		}
	}
	server := NewServer(store, Config{SigningSeed: testSigningSeed(), WSSTicketSigningSeed: bytes.Repeat([]byte{0x33}, 32), APIToken: testAPIToken})
	page := func() []string {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=1", nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", recorder.Code, recorder.Body.String())
		}
		var list relay.ListResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return relayIDs(list.Relays)
	}

	// Baseline: the reservation pulls the WSS relay into the one-slot page.
	if got := page(); len(got) != 1 || got[0] != wss.ID {
		t.Fatalf("without weights the WSS relay should be reserved into the page, got %v", got)
	}
	if response := operationalRequest(t, server, http.MethodPut, "/admin/api/relays/"+wss.ID+"/weight", `{"weight": 0}`); response.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", response.Code, response.Body.String())
	}
	if got := page(); len(got) != 1 || got[0] == wss.ID {
		t.Fatalf("drained WSS relay must not be reserved into the page, got %v", got)
	}
	// A partial weight keeps the functional reservation.
	if response := operationalRequest(t, server, http.MethodPut, "/admin/api/relays/"+wss.ID+"/weight", `{"weight": 0.1}`); response.Code != http.StatusOK {
		t.Fatalf("down-weight = %d: %s", response.Code, response.Body.String())
	}
	if got := page(); len(got) != 1 || got[0] != wss.ID {
		t.Fatalf("down-weighted WSS relay should still be reserved, got %v", got)
	}
}
