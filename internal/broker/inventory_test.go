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

const inventoryPath = "/admin/api/relays/inventory"

func TestRelayInventoryDisabledIsNotRegistered(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, inventoryPath, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled inventory status = %d, want 404", recorder.Code)
	}
}

func TestRelayInventoryAuthenticationAndCacheControl(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), InventoryToken: "inventory-secret"})
	for _, authorization := range []string{"", "inventory-secret", "Bearer  inventory-secret", "bearer inventory-secret", "Bearer wrong"} {
		t.Run(authorization, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestRelayInventoryReturnsCompleteSortedPublicSignedSet(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()
	for i := 0; i < 21; i++ {
		req := validRegisterRequest()
		req.PublicHost = "inventory-" + string(rune('a'+i)) + ".example"
		req.Label = "relay"
		if _, err := store.Register(req, now, time.Minute); err != nil {
			t.Fatalf("register relay %d: %v", i, err)
		}
	}
	server := NewServer(store, Config{SigningSeed: testSigningSeed(), InventoryToken: "inventory-secret"})
	req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
	req.Header.Set("Authorization", "Bearer inventory-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if recorder.Header().Get(signatureHeader) == "" {
		t.Fatal("successful inventory response was not signed")
	}
	var response inventoryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if response.Count != 21 || len(response.Relays) != 21 {
		t.Fatalf("count/relays = %d/%d, want 21/21", response.Count, len(response.Relays))
	}
	if response.Channel != "relay-inventory-v1" || response.ServerTime.IsZero() {
		t.Fatalf("unexpected inventory contract: %+v", response)
	}
	ids := make([]string, len(response.Relays))
	for i, desc := range response.Relays {
		ids[i] = desc.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("relay IDs are not sorted: %v", ids)
	}
	for _, forbidden := range []string{"lease_token", "identity_public_key", "exit_host", "identity_proof", "wss_capability_proof"} {
		if strings.Contains(recorder.Body.String(), "\""+forbidden+"\"") {
			t.Fatalf("inventory leaked private field %q: %s", forbidden, recorder.Body.String())
		}
	}
}

type failingInventoryStore struct{ RelayStore }

func (failingInventoryStore) List(time.Time, int) ([]relay.Descriptor, error) {
	return nil, errors.New("storage unavailable")
}

func TestRelayInventoryStorageFailureIsNeverCacheable(t *testing.T) {
	server := NewServer(failingInventoryStore{NewStore()}, Config{SigningSeed: testSigningSeed(), InventoryToken: "inventory-secret"})
	req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
	req.Header.Set("Authorization", "Bearer inventory-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestRelayInventoryRateLimited(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), InventoryToken: "inventory-secret"})
	for i := 0; i < relayInventoryBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
		req.Header.Set("Authorization", "Bearer inventory-secret")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, recorder.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
	req.Header.Set("Authorization", "Bearer inventory-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

type privateInventoryStore struct{ RelayStore }

func (privateInventoryStore) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	return []relay.Descriptor{{
		ID:                "relay-private-test",
		PublicHost:        "relay.example",
		PublicPort:        443,
		LeaseToken:        "must-not-leak",
		IdentityPublicKey: "must-not-leak",
		ExitHost:          "203.0.113.99",
		RegisteredAt:      now,
		LastHeartbeatAt:   now,
		ExpiresAt:         now.Add(time.Minute),
	}}, nil
}

func TestRelayInventoryDoesNotSerializePrivateDescriptorFields(t *testing.T) {
	server := NewServer(privateInventoryStore{NewStore()}, Config{SigningSeed: testSigningSeed(), InventoryToken: "inventory-secret"})
	req := httptest.NewRequest(http.MethodGet, inventoryPath, nil)
	req.Header.Set("Authorization", "Bearer inventory-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, private := range []string{"must-not-leak", "203.0.113.99"} {
		if strings.Contains(recorder.Body.String(), private) {
			t.Fatalf("inventory leaked private descriptor value %q: %s", private, recorder.Body.String())
		}
	}
}
