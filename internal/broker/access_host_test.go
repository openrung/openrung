// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

const (
	testAccessHost     = "2001:db8:1::10"
	testIPv4AccessHost = "198.51.100.9"
)

// TestRegisterStoresAccessHostPrivately covers the registration path end to
// end: an identity-bearing registration's access host is stored, shown in
// the operator inventory's operator-only block and on the admin relays page,
// and never reaches the registration response, the signed public directory
// or the inventory's public descriptor fields.
func TestRegisterStoresAccessHostPrivately(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)
	now := time.Now().UTC()

	req := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
		r.PublicHost = "203.0.113.7"
		r.AccessHost = " 2001:DB8:1:0::10 "
	}, now)
	registered := postRegistration(t, server, req)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registered.Code, registered.Body.String())
	}
	if body := registered.Body.String(); strings.Contains(body, testAccessHost) || strings.Contains(body, "access_host") {
		t.Fatalf("registration response echoed the access host: %s", body)
	}
	listed, err := store.List(now, 10)
	if err != nil {
		t.Fatalf("list stored relays: %v", err)
	}
	if len(listed) != 1 || listed[0].AccessHost != testAccessHost {
		t.Fatalf("stored access host = %+v, want the canonical %q", listed, testAccessHost)
	}

	list := httptest.NewRecorder()
	server.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if list.Code != http.StatusOK || list.Header().Get("X-OpenRung-Relays-Signature") == "" {
		t.Fatalf("list relays: %d, signed=%v: %s", list.Code, list.Header().Get("X-OpenRung-Relays-Signature") != "", list.Body.String())
	}
	if body := list.Body.String(); strings.Contains(body, testAccessHost) || strings.Contains(body, "access_host") {
		t.Fatalf("public relay list leaked the access host: %s", body)
	}

	inventory := getInventory(t, server)
	if inventory.Code != http.StatusOK {
		t.Fatalf("inventory: expected 200, got %d: %s", inventory.Code, inventory.Body.String())
	}
	var envelope struct {
		Relays []struct {
			PublicHost string `json:"public_host"`
			AccessHost string `json:"access_host"`
		} `json:"relays"`
	}
	if err := json.Unmarshal(inventory.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if len(envelope.Relays) != 1 || envelope.Relays[0].AccessHost != testAccessHost || envelope.Relays[0].PublicHost != "203.0.113.7" {
		t.Fatalf("inventory = %+v, want public host 203.0.113.7 and operator access_host %q", envelope.Relays, testAccessHost)
	}
	// The access host appears exactly once: in the operator block, never in
	// the descriptor fields the inventory shares with the public directory.
	if count := strings.Count(inventory.Body.String(), testAccessHost); count != 1 {
		t.Fatalf("access host appears %d times in the inventory: %s", count, inventory.Body.String())
	}

	panel := buildRelaysPanel(listed, relayTelemetryStats{}, nil, now, time.Hour)
	if len(panel.Relays) != 1 || panel.Relays[0].AccessHost != testAccessHost || panel.Relays[0].Endpoint != "203.0.113.7:443" {
		t.Fatalf("relays panel = %+v, want endpoint 203.0.113.7:443 with access host %q", panel.Relays, testAccessHost)
	}
}

// An IPv4 access address beside an IPv6 public host registers through the
// same path and stays out of the public directory.
func TestRegisterStoresIPv4AccessHostPrivately(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)
	now := time.Now().UTC()
	req := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
		r.PublicHost = "2001:db8:2::1"
		r.AccessHost = " " + testIPv4AccessHost + " "
	}, now)
	if recorder := postRegistration(t, server, req); recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	listed, err := store.List(now, 10)
	if err != nil || len(listed) != 1 || listed[0].AccessHost != testIPv4AccessHost || listed[0].PublicHost != "2001:db8:2::1" {
		t.Fatalf("stored = %+v, %v; want public 2001:db8:2::1 and access %q", listed, err, testIPv4AccessHost)
	}
	list := httptest.NewRecorder()
	server.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if body := list.Body.String(); list.Code != http.StatusOK || strings.Contains(body, testIPv4AccessHost) || strings.Contains(body, "access_host") {
		t.Fatalf("public relay list (%d) leaked the access host: %s", list.Code, body)
	}
}

func TestRegisterWithoutAccessHostOmitsIt(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)
	req := signedIdentityRequest(t, identityStoreSeedA, nil, time.Now().UTC())
	if recorder := postRegistration(t, server, req); recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if inventory := getInventory(t, server); strings.Contains(inventory.Body.String(), "access_host") {
		t.Fatalf("inventory carries an empty access_host: %s", inventory.Body.String())
	}
}

func TestRegisterRejectsInvalidAccessHost(t *testing.T) {
	for name, mutate := range map[string]func(*relay.RegisterRequest){
		"private ipv4":   func(r *relay.RegisterRequest) { r.AccessHost = "10.0.0.9" },
		"cgnat ipv4":     func(r *relay.RegisterRequest) { r.AccessHost = "100.64.0.9" },
		"ipv4-mapped":    func(r *relay.RegisterRequest) { r.AccessHost = "::ffff:198.51.100.9" },
		"ipv4 public":    func(r *relay.RegisterRequest) { r.PublicHost = "198.51.100.9"; r.AccessHost = "198.51.100.9" },
		"unique local":   func(r *relay.RegisterRequest) { r.AccessHost = "fd00::10" },
		"link local":     func(r *relay.RegisterRequest) { r.AccessHost = "fe80::10" },
		"bracketed":      func(r *relay.RegisterRequest) { r.AccessHost = "[" + testAccessHost + "]" },
		"zoned":          func(r *relay.RegisterRequest) { r.AccessHost = testAccessHost + "%eth0" },
		"dns name":       func(r *relay.RegisterRequest) { r.AccessHost = "access.example.com" },
		"control bytes":  func(r *relay.RegisterRequest) { r.AccessHost = testAccessHost + "\x1b[2J" },
		"oversized":      func(r *relay.RegisterRequest) { r.AccessHost = strings.Repeat("2001:", 200) + ":1" },
		"public host":    func(r *relay.RegisterRequest) { r.PublicHost = "2001:db8:1:0:0:0:0:10"; r.AccessHost = testAccessHost },
		"tunnel relayed": func(r *relay.RegisterRequest) { r.Transport = relay.TransportTunnel; r.AccessHost = testAccessHost },
	} {
		server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
		req := signedIdentityRequest(t, identityStoreSeedA, mutate, time.Now().UTC())
		if recorder := postRegistration(t, server, req); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

// The broker's reachability probe targets the public endpoint, never the
// access address.
func TestRegistrationProbesThePublicEndpointNotTheAccessHost(t *testing.T) {
	prober := &stubProber{}
	server := probeServer(t, prober, time.Hour)
	req := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
		r.PublicHost = "203.0.113.7"
		r.AccessHost = testAccessHost
	}, time.Now().UTC())
	if recorder := postRegistration(t, server, req); recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	prober.mu.Lock()
	defer prober.mu.Unlock()
	if len(prober.endpoints) != 1 || prober.endpoints[0] != "203.0.113.7:443" {
		t.Fatalf("probed endpoints = %v, want only the public 203.0.113.7:443", prober.endpoints)
	}
}

// Store conformance: an identity-bearing registration keeps its access host
// through heartbeats and listing, a re-registration without one clears it,
// and a legacy identityless registration can never set it.
func TestStoreAccessHostFollowsIdentityRegistration(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
		withAccess := func(r *relay.RegisterRequest) { r.AccessHost = testAccessHost }
		desc, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, withAccess, now), now, time.Minute)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if desc.AccessHost != testAccessHost {
			t.Fatalf("registered access host = %q, want %q", desc.AccessHost, testAccessHost)
		}
		beat, err := store.Heartbeat(desc.ID, desc.LeaseToken, relay.NodeClassVolunteer, "", now.Add(10*time.Second), time.Minute)
		if err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if beat.AccessHost != testAccessHost {
			t.Fatalf("heartbeat lost the access host: %q", beat.AccessHost)
		}
		got, err := store.RelayByID(desc.ID, now.Add(20*time.Second))
		if err != nil || got.AccessHost != testAccessHost {
			t.Fatalf("get = %q, %v; want the access host", got.AccessHost, err)
		}
		listed, _, err := store.ListRanked(now.Add(20*time.Second), 0)
		if err != nil || len(listed) != 1 || listed[0].AccessHost != testAccessHost {
			t.Fatalf("ranked list = %+v, %v; want the access host", listed, err)
		}

		later := now.Add(30 * time.Second)
		if _, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, later), later, time.Minute); err != nil {
			t.Fatalf("re-register: %v", err)
		}
		listed, err = store.List(later, 10)
		if err != nil || len(listed) != 1 || listed[0].AccessHost != "" {
			t.Fatalf("re-registration without an access host kept one: %+v, %v", listed, err)
		}

		legacy := validRegisterRequest()
		legacy.PublicHost = "legacy.example.com"
		legacy.AccessHost = testAccessHost
		legacyDesc, err := store.Register(legacy, later, time.Minute)
		if err != nil {
			t.Fatalf("register legacy: %v", err)
		}
		if legacyDesc.AccessHost != "" {
			t.Fatalf("legacy identityless registration set an access host: %q", legacyDesc.AccessHost)
		}
		if got, err := store.RelayByID(legacyDesc.ID, later); err != nil || got.AccessHost != "" {
			t.Fatalf("stored legacy access host = %q, %v; want none", got.AccessHost, err)
		}
	})
}

// The access answer names the access host in place of the public host only
// for a request that arrived over IPv6: an IPv4 (or IPv4-mapped) request gets
// the public host it can reach. Everything else — the lease credential
// included — is the relay's descriptor either way, relays without an access
// host keep their public host, and the signed body still passes the client's
// own wire validation.
func TestAccessAnswerSubstitutesAccessHostForIPv6Requests(t *testing.T) {
	h := newAccessHarness(t)
	h.handler.clientIP = func(r *http.Request) string { return peerIP(r.RemoteAddr) }
	withAccess := h.addRelay("v6", 'a', "FI", func(r *relay.RegisterRequest) {
		r.PublicHost = "203.0.113.7"
		r.AccessHost = testAccessHost
	})
	plain := h.addRelay("plain", 'b', "DE", nil)
	h.autoAck(withAccess)
	h.autoAck(plain)
	token, userID := h.token()

	for _, tc := range []struct {
		source        string
		wantAccessHit bool
	}{
		{"[2001:db8:9::5]:4000", true},
		{"203.0.113.10:4000", false},
		{"[::ffff:203.0.113.10]:4000", false},
	} {
		h.source = tc.source
		rec, resp := h.request(token, `{}`)
		if rec.Code != http.StatusOK || len(resp.Relays) != 2 {
			t.Fatalf("%s: access: %d %s", tc.source, rec.Code, rec.Body.String())
		}
		credentials := map[string]string{}
		for _, lease := range h.liveLeases(userID) {
			credentials[lease.RelayID] = lease.Credential
		}
		for _, desc := range resp.Relays {
			registered := h.store.relays[desc.ID]
			wantHost := registered.PublicHost
			if desc.ID == withAccess.ID && tc.wantAccessHit {
				wantHost = testAccessHost
			}
			if desc.PublicHost != wantHost {
				t.Fatalf("%s: relay %s public_host = %q, want %q", tc.source, desc.ID, desc.PublicHost, wantHost)
			}
			if desc.ClientID != credentials[desc.ID] {
				t.Fatalf("%s: relay %s client_id = %q, want its lease credential", tc.source, desc.ID, desc.ClientID)
			}
			if desc.PublicPort != registered.PublicPort || desc.RealityPublicKey != registered.RealityPublicKey ||
				desc.ShortID != registered.ShortID || desc.ServerName != registered.ServerName || desc.NodeClass != registered.NodeClass {
				t.Fatalf("%s: relay %s descriptor changed beyond the host: %+v vs %+v", tc.source, desc.ID, desc, registered)
			}
		}
		if strings.Contains(rec.Body.String(), "access_host") {
			t.Fatalf("%s: access answer carried an access_host field: %s", tc.source, rec.Body.String())
		}
	}
	// The stored descriptor and the public directory keep the public host.
	if stored := h.store.relays[withAccess.ID]; stored.PublicHost != "203.0.113.7" {
		t.Fatalf("substitution leaked into the store: %q", stored.PublicHost)
	}

	// Without a resolver nothing is substituted.
	h.handler.clientIP = nil
	h.source = "[2001:db8:9::5]:4000"
	if _, resp := h.request(token, `{"relay_id":"`+withAccess.ID+`"}`); len(resp.Relays) != 1 || resp.Relays[0].PublicHost != "203.0.113.7" {
		t.Fatalf("access answer without a resolver = %+v, want the public host", resp.Relays)
	}
	h.handler.clientIP = func(r *http.Request) string { return peerIP(r.RemoteAddr) }

	// The real client path over IPv6 loopback: brokerapi's access verifier
	// applies the relay-list wire schema (the signature is checked by request
	// above).
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback for the client path: %v", err)
	}
	server := httptest.NewUnstartedServer(h.server)
	server.Listener = listener
	server.Start()
	defer server.Close()
	client := brokerapi.NewClient(server.Client(), brokerapi.Options{})
	answer, err := client.RequestAccess(context.Background(), server.URL, brokerapi.AccessRequest{AccessToken: token, RelayID: withAccess.ID})
	if err != nil {
		t.Fatalf("client RequestAccess: %v", err)
	}
	if len(answer.Relays) != 1 || answer.Relays[0].PublicHost != testAccessHost {
		t.Fatalf("client-decoded access answer = %+v", answer.Relays)
	}
}

// An IPv4 access address is stored like an IPv6 one and, in the access
// answer, replaces the public host whatever family the request arrived over;
// an IPv6 access address keeps the IPv6-only rule. Both are checked here
// across IPv4 and IPv6 requests.
func TestAccessAnswerSubstitutionByAccessAddressFamily(t *testing.T) {
	h := newAccessHarness(t)
	h.handler.clientIP = func(r *http.Request) string { return peerIP(r.RemoteAddr) }
	v4Access := h.addRelay("v4", 'a', "FI", func(r *relay.RegisterRequest) {
		r.PublicHost = "2001:db8:2::1"
		r.AccessHost = testIPv4AccessHost
	})
	v6Access := h.addRelay("v6", 'b', "DE", func(r *relay.RegisterRequest) {
		r.PublicHost = "203.0.113.7"
		r.AccessHost = testAccessHost
	})
	h.autoAck(v4Access)
	h.autoAck(v6Access)
	if stored := h.store.relays[v4Access.ID]; stored.AccessHost != testIPv4AccessHost {
		t.Fatalf("stored IPv4 access host = %q, want %q", stored.AccessHost, testIPv4AccessHost)
	}
	token, _ := h.token()

	for _, tc := range []struct {
		source string
		want   map[string]string
	}{
		{"[2001:db8:9::5]:4000", map[string]string{v4Access.ID: testIPv4AccessHost, v6Access.ID: testAccessHost}},
		{"203.0.113.10:4000", map[string]string{v4Access.ID: testIPv4AccessHost, v6Access.ID: "203.0.113.7"}},
		{"[::ffff:203.0.113.10]:4000", map[string]string{v4Access.ID: testIPv4AccessHost, v6Access.ID: "203.0.113.7"}},
	} {
		h.source = tc.source
		rec, resp := h.request(token, `{}`)
		if rec.Code != http.StatusOK || len(resp.Relays) != 2 {
			t.Fatalf("%s: access: %d %s", tc.source, rec.Code, rec.Body.String())
		}
		for _, desc := range resp.Relays {
			if desc.PublicHost != tc.want[desc.ID] {
				t.Fatalf("%s: relay %s public_host = %q, want %q", tc.source, desc.ID, desc.PublicHost, tc.want[desc.ID])
			}
		}
	}
	// The stored descriptor keeps the public host.
	if stored := h.store.relays[v4Access.ID]; stored.PublicHost != "2001:db8:2::1" {
		t.Fatalf("substitution leaked into the store: %q", stored.PublicHost)
	}
}

func TestRequestOverIPv6(t *testing.T) {
	for source, want := range map[string]bool{
		"2001:db8::5":         true,
		"203.0.113.10":        false,
		"::ffff:203.0.113.10": false,
		"":                    false,
		"not-an-address":      false,
		" 2001:db8::5 ":       true,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/access", nil)
		got := requestOverIPv6(func(*http.Request) string { return source }, r)
		if got != want {
			t.Errorf("requestOverIPv6(%q) = %v, want %v", source, got, want)
		}
	}
	if requestOverIPv6(nil, httptest.NewRequest(http.MethodPost, "/api/v1/access", nil)) {
		t.Error("a nil resolver counted as IPv6")
	}
}
