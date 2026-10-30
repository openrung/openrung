// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

const testOperatorContact = "ops@example.org"

func postRegistration(t *testing.T, handler http.Handler, req relay.RegisterRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	return recorder
}

// TestRegisterStoresOperatorContactPrivately covers the whole path: the
// contact is stored, appears in the operator inventory, and never reaches the
// registration response or the signed public directory.
func TestRegisterStoresOperatorContactPrivately(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)

	req := validRegisterRequest()
	req.OperatorContact = "  " + testOperatorContact + " "
	registered := postRegistration(t, server, req)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registered.Code, registered.Body.String())
	}
	if strings.Contains(registered.Body.String(), testOperatorContact) {
		t.Fatalf("registration response echoed the operator contact: %s", registered.Body.String())
	}
	listed, err := store.List(time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("list stored relays: %v", err)
	}
	if len(listed) != 1 || listed[0].OperatorContact != testOperatorContact {
		t.Fatalf("stored operator contact = %+v, want %q", listed, testOperatorContact)
	}
	stored := listed[0]

	list := httptest.NewRecorder()
	server.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list relays: expected 200, got %d: %s", list.Code, list.Body.String())
	}
	if list.Header().Get("X-OpenRung-Relays-Signature") == "" {
		t.Fatal("public relay list is unsigned; the leak check would not cover the signed body")
	}
	if body := list.Body.String(); strings.Contains(body, testOperatorContact) || strings.Contains(body, "operator_contact") {
		t.Fatalf("public relay list leaked the operator contact: %s", body)
	}

	inventory := getInventory(t, server)
	if inventory.Code != http.StatusOK {
		t.Fatalf("inventory: expected 200, got %d: %s", inventory.Code, inventory.Body.String())
	}
	var envelope struct {
		Relays []struct {
			ID              string `json:"id"`
			OperatorContact string `json:"operator_contact"`
		} `json:"relays"`
	}
	if err := json.Unmarshal(inventory.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if len(envelope.Relays) != 1 || envelope.Relays[0].OperatorContact != testOperatorContact {
		t.Fatalf("inventory operator contact = %+v, want %q", envelope.Relays, testOperatorContact)
	}

	panel := buildRelaysPanel([]relay.Descriptor{stored}, relayTelemetryStats{}, RankingInputs{}, false, time.Now().UTC(), time.Hour)
	if len(panel.Relays) != 1 || panel.Relays[0].OperatorContact != testOperatorContact {
		t.Fatalf("relays panel operator contact = %+v, want %q", panel.Relays, testOperatorContact)
	}
}

// TestRegisterWithoutOperatorContact keeps the field optional: relays that
// predate it (and installs that skipped the prompt) register as before, and
// the inventory omits the key.
func TestRegisterWithoutOperatorContact(t *testing.T) {
	store := NewStore()
	server := inventoryServer(t, store)
	if recorder := postRegistration(t, server, validRegisterRequest()); recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	inventory := getInventory(t, server)
	if strings.Contains(inventory.Body.String(), "operator_contact") {
		t.Fatalf("inventory carries an empty operator_contact: %s", inventory.Body.String())
	}
}

func TestRegisterRejectsInvalidOperatorContact(t *testing.T) {
	for _, contact := range []string{
		strings.Repeat("a", relay.MaxOperatorContactLength) + "@example.org",
		"not-an-email",
		"ops@example",
		"ops @example.org",
		"ops\x1b[2J@example.org",
	} {
		server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
		req := validRegisterRequest()
		req.OperatorContact = contact
		if recorder := postRegistration(t, server, req); recorder.Code != http.StatusBadRequest {
			t.Errorf("contact %q: expected 400, got %d: %s", contact, recorder.Code, recorder.Body.String())
		}
	}
}

// TestStoreOperatorContactFollowsRegistration checks the memory store keeps
// the contact and that a re-registration of the endpoint replaces it.
func TestStoreOperatorContactFollowsRegistration(t *testing.T) {
	testOperatorContactRoundTrip(t, NewStore())
}

func TestPostgresStoreRoundTripsOperatorContact(t *testing.T) {
	testOperatorContactRoundTrip(t, newTestPostgresStore(t))
}

func testOperatorContactRoundTrip(t *testing.T, store RelayStore) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	req := validRegisterRequest()
	req.OperatorContact = testOperatorContact
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.OperatorContact != testOperatorContact {
		t.Fatalf("registered operator contact = %q, want %q", desc.OperatorContact, testOperatorContact)
	}
	listed, err := store.List(now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].OperatorContact != testOperatorContact {
		t.Fatalf("listed operator contact not preserved: %+v", listed)
	}

	req.OperatorContact = ""
	if _, err := store.Register(req, now.Add(2*time.Second), time.Minute); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	listed, err = store.List(now.Add(3*time.Second), 10)
	if err != nil {
		t.Fatalf("list after re-registration: %v", err)
	}
	if len(listed) != 1 || listed[0].OperatorContact != "" {
		t.Fatalf("re-registration kept a stale operator contact: %+v", listed)
	}
}
