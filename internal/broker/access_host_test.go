// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

const testAccessHost = "2001:db8:1::10"

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
		"ipv4":           func(r *relay.RegisterRequest) { r.AccessHost = "198.51.100.9" },
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

// The access answer names the access host in place of the public host for a
// relay that has one, keeps everything else (the lease credential included)
// exactly as before, and leaves relays without one on their public host. The
// signed body still passes the client's own wire validation.
func TestAccessAnswerSubstitutesAccessHost(t *testing.T) {
	h := newAccessHarness(t)
	withAccess := h.addRelay("v6", 'a', "FI", func(r *relay.RegisterRequest) {
		r.PublicHost = "203.0.113.7"
		r.AccessHost = testAccessHost
	})
	plain := h.addRelay("plain", 'b', "DE", nil)
	h.autoAck(withAccess)
	h.autoAck(plain)

	token, userID := h.token()
	rec, resp := h.request(token, `{}`)
	if rec.Code != http.StatusOK || len(resp.Relays) != 2 {
		t.Fatalf("access: %d %s", rec.Code, rec.Body.String())
	}
	credentials := map[string]string{}
	for _, lease := range h.liveLeases(userID) {
		credentials[lease.RelayID] = lease.Credential
	}
	for _, desc := range resp.Relays {
		registered := h.store.relays[desc.ID]
		wantHost := registered.PublicHost
		if desc.ID == withAccess.ID {
			wantHost = testAccessHost
		}
		if desc.PublicHost != wantHost {
			t.Fatalf("relay %s public_host = %q, want %q", desc.ID, desc.PublicHost, wantHost)
		}
		if desc.ClientID != credentials[desc.ID] {
			t.Fatalf("relay %s client_id = %q, want its lease credential", desc.ID, desc.ClientID)
		}
		if desc.PublicPort != registered.PublicPort || desc.RealityPublicKey != registered.RealityPublicKey ||
			desc.ShortID != registered.ShortID || desc.ServerName != registered.ServerName || desc.NodeClass != registered.NodeClass {
			t.Fatalf("relay %s descriptor changed beyond the host: %+v vs %+v", desc.ID, desc, registered)
		}
	}
	if strings.Contains(rec.Body.String(), "access_host") {
		t.Fatalf("access answer carried an access_host field: %s", rec.Body.String())
	}
	// The stored descriptor and the public directory keep the public host.
	if stored := h.store.relays[withAccess.ID]; stored.PublicHost != "203.0.113.7" {
		t.Fatalf("substitution leaked into the store: %q", stored.PublicHost)
	}

	// The real client path over loopback: brokerapi's access verifier applies
	// the relay-list wire schema (the signature is checked by request above).
	server := httptest.NewServer(h.server)
	defer server.Close()
	client := brokerapi.NewClient(server.Client(), brokerapi.Options{})
	answer, err := client.RequestAccess(context.Background(), server.URL, brokerapi.AccessRequest{AccessToken: token, RelayID: withAccess.ID})
	if err != nil {
		t.Fatalf("client RequestAccess: %v", err)
	}
	if len(answer.Relays) != 1 || answer.Relays[0].PublicHost != testAccessHost || answer.Relays[0].ClientID != credentials[withAccess.ID] {
		t.Fatalf("client-decoded access answer = %+v", answer.Relays)
	}
}
