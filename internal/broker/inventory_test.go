package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

const testAPIToken = "operational-api-token"

func inventoryServer(t *testing.T, store RelayStore) http.Handler {
	t.Helper()
	return NewServer(store, Config{SigningSeed: testSigningSeed(), APIToken: testAPIToken})
}

// getInventory issues an authorized inventory request against handler.
func getInventory(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/relays/inventory", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// registerInventoryFleet registers count relays on distinct endpoints, each
// with a distinct heartbeat time so the store's candidate ranking (which
// tie-breaks on heartbeat recency) is deliberately NOT relay-ID order.
func registerInventoryFleet(t *testing.T, store RelayStore, count int) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		req := validRegisterRequest()
		req.PublicPort = 20000 + i
		// Stagger backwards so relay 0 is the most recently seen; ranking then
		// orders by registration order, which is uncorrelated with the random
		// hex relay IDs the store mints.
		at := now.Add(-time.Duration(i) * time.Second)
		if _, err := store.Register(req, at, time.Hour); err != nil {
			t.Fatalf("register relay %d: %v", i, err)
		}
	}
}

func decodeInventory(t *testing.T, body []byte) relay.ListResponse {
	t.Helper()
	var out relay.ListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode inventory: %v: %s", err, body)
	}
	return out
}

// TestRelayInventoryRouteAbsentWithoutAPIToken pins the fail-closed posture:
// without its dedicated credential the route is never registered, so the
// endpoint cannot be probed for existence, let alone served open.
func TestRelayInventoryRouteAbsentWithoutAPIToken(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no tokens at all", cfg: Config{SigningSeed: testSigningSeed()}},
		{
			// The dashboard credential must not reach across to the
			// machine-to-machine API.
			name: "dashboard token only",
			cfg:  Config{SigningSeed: testSigningSeed(), DashboardToken: "dashboard-token"},
		},
		{
			name: "registration tokens only",
			cfg:  Config{SigningSeed: testSigningSeed(), RegistrationToken: "volunteer", FoundationToken: "foundation"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(NewStore(), test.cfg)
			for _, authorization := range []string{"", "Bearer " + testAPIToken, "Bearer dashboard-token"} {
				request := httptest.NewRequest(http.MethodGet, "/admin/api/relays/inventory", nil)
				if authorization != "" {
					request.Header.Set("Authorization", authorization)
				}
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("Authorization %q: expected 404 with no API token configured, got %d: %s",
						authorization, recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}

// TestRelayInventoryRejectsInvalidCredentials covers the whole bad-bearer
// surface, including the length-adjacent and case variants a sloppy comparison
// would accept.
func TestRelayInventoryRejectsInvalidCredentials(t *testing.T) {
	store := NewStore()
	registerInventoryFleet(t, store, 3)

	tests := []struct {
		name          string
		authorization string
	}{
		{name: "missing header"},
		{name: "bare token without scheme", authorization: testAPIToken},
		{name: "wrong token", authorization: "Bearer wrong-token"},
		{name: "truncated token", authorization: "Bearer " + testAPIToken[:len(testAPIToken)-1]},
		{name: "extended token", authorization: "Bearer " + testAPIToken + "x"},
		{name: "lowercase scheme", authorization: "bearer " + testAPIToken},
		{name: "doubled scheme separator", authorization: "Bearer  " + testAPIToken},
		{name: "wrong scheme", authorization: "Basic " + testAPIToken},
		{name: "empty bearer", authorization: "Bearer "},
		{name: "dashboard-style token", authorization: "Bearer dashboard-token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A server per case: every request here shares one client IP, so a
			// single server would spend the endpoint's small rate-limit burst
			// across the table and make the next case added fail as a 429.
			server := inventoryServer(t, store)
			request := httptest.NewRequest(http.MethodGet, "/admin/api/relays/inventory", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := recorder.Header().Get(signatureHeader); got != "" {
				t.Errorf("401 response must not be signed, got header %q", got)
			}
			if strings.Contains(recorder.Body.String(), "relay_") {
				t.Errorf("401 body leaked relay data: %s", recorder.Body.String())
			}
		})
	}
}

// TestRelayInventoryMethodHandlingLeaksNothingAndStaysUncacheable pins the
// reason the route is registered without a method pattern: ServeMux would
// answer a method mismatch itself, ahead of the handler — with no Cache-Control
// and, worse, a 405 that tells an anonymous prober the endpoint exists on a
// broker where an unset token should make it indistinguishable from any other
// missing path.
func TestRelayInventoryMethodHandlingLeaksNothingAndStaysUncacheable(t *testing.T) {
	store := NewStore()
	registerInventoryFleet(t, store, 2)
	disabled := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	// A server per subtest: every request here shares one client IP, and the
	// endpoint's rate-limit burst is deliberately small.
	newServer := func() http.Handler { return inventoryServer(t, store) }

	otherMethods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions}
	for _, method := range otherMethods {
		t.Run(strings.ToLower(method)+" without a credential", func(t *testing.T) {
			server := newServer()
			// Same answer as an unauthorized GET: the method must not be an
			// unauthenticated oracle for the endpoint's existence.
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(method, "/admin/api/relays/inventory", nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Allow"); got != "" {
				t.Errorf("unauthorized response advertised Allow: %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}

			// And on a broker with no token configured, still a plain 404.
			disabledRecorder := httptest.NewRecorder()
			disabled.ServeHTTP(disabledRecorder, httptest.NewRequest(method, "/admin/api/relays/inventory", nil))
			if disabledRecorder.Code != http.StatusNotFound {
				t.Fatalf("token unset: expected 404, got %d", disabledRecorder.Code)
			}
		})

		t.Run(strings.ToLower(method)+" with a credential", func(t *testing.T) {
			server := newServer()
			request := httptest.NewRequest(method, "/admin/api/relays/inventory", nil)
			request.Header.Set("Authorization", "Bearer "+testAPIToken)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected 405, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
			// 405 is cacheable by default under RFC 9110; an edge storing one
			// would replay it to every other operator.
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if strings.Contains(recorder.Body.String(), "relay_") {
				t.Errorf("405 body leaked relay data: %s", recorder.Body.String())
			}
		})
	}

	t.Run("head is served like get", func(t *testing.T) {
		server := newServer()
		request := httptest.NewRequest(http.MethodHead, "/admin/api/relays/inventory", nil)
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get(signatureHeader); got == "" {
			t.Error("HEAD response carries no signature header")
		}
	})
}

// TestRelayInventoryReturnsWholeFleetInRelayIDOrder is the endpoint's reason
// for existing: every active relay, past the client page size, in an order
// that depends only on which relays exist.
func TestRelayInventoryReturnsWholeFleetInRelayIDOrder(t *testing.T) {
	const fleetSize = 25 // deliberately past the client list's limit of 20
	store := NewStore()
	registerInventoryFleet(t, store, fleetSize)
	server := inventoryServer(t, store)

	recorder := getInventory(t, server)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	inventory := decodeInventory(t, recorder.Body.Bytes())

	if inventory.Count != fleetSize || len(inventory.Relays) != fleetSize {
		t.Fatalf("inventory count=%d len=%d, want %d of both", inventory.Count, len(inventory.Relays), fleetSize)
	}

	ids := make([]string, len(inventory.Relays))
	for i, desc := range inventory.Relays {
		ids[i] = desc.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("inventory is not in ascending relay-ID order: %v", ids)
	}

	// Every registered relay is present exactly once.
	ranked, err := store.List(time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if len(ranked) != fleetSize {
		t.Fatalf("store holds %d active relays, want %d", len(ranked), fleetSize)
	}
	seen := make(map[string]int, len(ids))
	for _, id := range ids {
		seen[id]++
	}
	for _, desc := range ranked {
		if seen[desc.ID] != 1 {
			t.Errorf("relay %s appears %d times in the inventory, want exactly 1", desc.ID, seen[desc.ID])
		}
	}

	// Guard the assertion above against a trivially-passing fixture: the
	// inventory order must be the ID order, which here is NOT the ranking.
	rankedIDs := make([]string, len(ranked))
	for i, desc := range ranked {
		rankedIDs[i] = desc.ID
	}
	if sort.StringsAreSorted(rankedIDs) {
		t.Fatalf("fixture is degenerate: ranking already happens to be ID order (%v)", rankedIDs)
	}

	// Sorting the inventory must not have disturbed the ranked slice the
	// client directory depends on.
	reranked, err := store.List(time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("re-list relays: %v", err)
	}
	for i, desc := range reranked {
		if desc.ID != rankedIDs[i] {
			t.Fatalf("ranked order changed after the inventory read: position %d is %s, was %s", i, desc.ID, rankedIDs[i])
		}
	}

	// The client directory still pages; the inventory is the untruncated set.
	pageRecorder := httptest.NewRecorder()
	server.ServeHTTP(pageRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=20", nil))
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("client relay list: expected 200, got %d: %s", pageRecorder.Code, pageRecorder.Body.String())
	}
	page := decodeInventory(t, pageRecorder.Body.Bytes())
	if len(page.Relays) != 20 {
		t.Fatalf("client relay list returned %d relays, want the 20-relay maximum page", len(page.Relays))
	}
}

// TestRelayInventoryIsSignedOnTheInventoryChannel pins the wire contract: the
// same relay-list signature over the exact bytes, but a channel of its own so
// an operator snapshot can never be replayed into a client's slot.
func TestRelayInventoryIsSignedOnTheInventoryChannel(t *testing.T) {
	vectors := loadSigningVectors(t)
	store := NewStore()
	registerInventoryFleet(t, store, 2)

	recorder := getInventory(t, inventoryServer(t, store))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	headerKeyID := verifyRelayListSignature(t, recorder.Header().Get(signatureHeader), body, vectors.SpecVector.PubkeyHex)
	// The inventory shares the directory's timestamp narrowing, so an operator
	// tool written against the relay list needs no second date parser.
	assertRelayListUsesWholeSecondUTCTimestamps(t, body)
	if headerKeyID != vectors.SpecVector.KeyID {
		t.Errorf("header key_id = %q, want %q", headerKeyID, vectors.SpecVector.KeyID)
	}

	inventory := decodeInventory(t, body)
	if inventory.Channel != relay.ChannelInventory {
		t.Errorf("channel = %q, want %q", inventory.Channel, relay.ChannelInventory)
	}
	if inventory.Channel == relay.ChannelAPI || inventory.Channel == relay.ChannelMirror {
		t.Fatal("the inventory channel must be distinct from the client channels")
	}
	if inventory.KeyID != vectors.SpecVector.KeyID {
		t.Errorf("body key_id = %q, want %q", inventory.KeyID, vectors.SpecVector.KeyID)
	}
	if inventory.ServerTime.IsZero() {
		t.Error("inventory carries no server_time")
	}
	if want := inventory.ServerTime.Add(inventoryNotAfterWindow); !inventory.NotAfter.Equal(want) {
		t.Errorf("not_after = %s, want %s", inventory.NotAfter, want)
	}
	// Not request-shaped: no limit to echo, so the field must be absent
	// entirely rather than present and zero.
	if strings.Contains(string(body), `"limit"`) {
		t.Errorf("inventory body must not carry a limit field: %s", body)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, no-transform" {
		t.Errorf("Cache-Control = %q, want no-store, no-transform", got)
	}
}

// TestRelayInventoryOmitsPrivateDescriptorFields fixtures a relay carrying
// every piece of private material a descriptor can hold — lease token, stable
// identity key, hub-observed exit address — and asserts none of it reaches the
// wire. The allowlist is deliberate: a new descriptor field must be reviewed
// here before it can appear in an operator snapshot.
func TestRelayInventoryOmitsPrivateDescriptorFields(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()
	req := signedIdentityRequest(t, identityStoreSeedA, func(req *relay.RegisterRequest) {
		req.Transport = relay.TransportTunnel
		req.ExitHost = "203.0.113.9"
		req.Label = "fixture-relay"
	}, now)
	desc, err := store.Register(req, now, time.Hour)
	if err != nil {
		t.Fatalf("register identity relay: %v", err)
	}
	// The fixture is only meaningful if the store really is holding secrets.
	if desc.LeaseToken == "" || desc.IdentityPublicKey == "" || desc.ExitHost == "" {
		t.Fatalf("fixture relay carries no private material: lease=%q identity=%q exit=%q",
			desc.LeaseToken, desc.IdentityPublicKey, desc.ExitHost)
	}

	recorder := getInventory(t, inventoryServer(t, store))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()

	for _, secret := range []string{desc.LeaseToken, desc.IdentityPublicKey, desc.ExitHost, req.IdentityProof} {
		if strings.Contains(body, secret) {
			t.Errorf("inventory body leaked private value %q: %s", secret, body)
		}
	}

	var envelope struct {
		Relays []map[string]json.RawMessage `json:"relays"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode inventory relays: %v", err)
	}
	if len(envelope.Relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(envelope.Relays))
	}
	publicFields := map[string]bool{
		"id": true, "label": true, "public_host": true, "public_port": true,
		"city": true, "country": true, "country_code": true, "latitude": true, "longitude": true,
		"node_class": true, "protocol": true, "client_id": true, "reality_public_key": true,
		"short_id": true, "server_name": true, "flow": true, "exit_mode": true,
		"max_sessions": true, "max_mbps": true, "relay_version": true, "volunteer_version": true,
		"transport": true, "punch_capable": true, "punch_endpoint": true, "wss_fronts": true,
		"registered_at": true, "last_heartbeat_at": true, "expires_at": true,
	}
	for field := range envelope.Relays[0] {
		if !publicFields[field] {
			t.Errorf("inventory descriptor carries non-public field %q: %s", field, body)
		}
	}
}

// TestRelayInventoryStorageFailureStaysUncacheable keeps the failure path from
// being the one an edge caches and replays to every other operator.
func TestRelayInventoryStorageFailureStaysUncacheable(t *testing.T) {
	store := failingStore{Store: NewStore(), listErr: errors.New("database down")}
	recorder := getInventory(t, inventoryServer(t, store))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get(signatureHeader); got != "" {
		t.Errorf("503 response must not be signed, got header %q", got)
	}
	if strings.Contains(recorder.Body.String(), "database down") {
		t.Errorf("503 body leaked the storage error: %s", recorder.Body.String())
	}
}

// TestRelayInventoryEmptyFleetEncodesAsJSONArray matches the public channels:
// a store with no rows must still produce "relays": [], never null.
func TestRelayInventoryEmptyFleetEncodesAsJSONArray(t *testing.T) {
	recorder := getInventory(t, inventoryServer(t, nilListStore{Store: NewStore()}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Count  int             `json:"count"`
		Relays json.RawMessage `json:"relays"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body.Relays) != "[]" {
		t.Fatalf("relays = %s, want []", body.Relays)
	}
	if body.Count != 0 {
		t.Fatalf("count = %d, want 0", body.Count)
	}
}

// TestRelayInventoryIsRateLimited confirms a leaked token cannot be turned
// into an amplification lever: each request is a full unbounded store read.
func TestRelayInventoryIsRateLimited(t *testing.T) {
	store := NewStore()
	registerInventoryFleet(t, store, 1)
	server := inventoryServer(t, store)

	var recorder *httptest.ResponseRecorder
	for i := 0; i < inventoryBurst+1; i++ {
		recorder = getInventory(t, server)
		if i < inventoryBurst && recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d inside the burst was limited", i+1)
		}
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 past the burst, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("429 Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Error("429 response carries no Retry-After header")
	}
}
