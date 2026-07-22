package broker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/wssbridge"
)

func TestWSSTicketBindsExactLiveRelayAndAdvertisedFront(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	desc := registerWSSRelayForTest(t, store, now, 3*time.Minute)
	seed := bytes.Repeat([]byte{0x73}, ed25519.SeedSize)
	issuer := newWSSTicketIssuer(Config{WSSTicketSigningSeed: seed})
	issuer.now = func() time.Time { return now }

	recorder := requestWSSTicket(t, store, issuer, relay.WSSSessionTicketRequest{
		RelayID: desc.ID, FrontID: "front-a",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response relay.WSSSessionTicketResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.URL != desc.WSSFronts[0].URL {
		t.Fatalf("URL = %q, want %q", response.URL, desc.WSSFronts[0].URL)
	}
	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	verifier, err := wssbridge.NewTicketVerifier(
		map[string]ed25519.PublicKey{wssbridge.TicketKeyID(publicKey): publicKey},
		desc.ID,
		wssbridge.TicketOptions{Now: func() time.Time { return now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(response.Ticket)
	if err != nil {
		t.Fatalf("verify ticket: %v", err)
	}
	if claims.RelayID != desc.ID || claims.FrontID != "front-a" || claims.MaxStreams != defaultWSSTicketStreams {
		t.Fatalf("claims = %+v", claims)
	}
	if strings.Contains(response.Ticket, desc.PublicHost) || strings.Contains(recorder.Body.String(), `"target`) {
		t.Fatal("ticket response exposed a dial target")
	}
}

func TestWSSTicketRejectsUnadvertisedFrontAndExpiredRelay(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	desc := registerWSSRelayForTest(t, store, now, time.Minute)
	issuer := newWSSTicketIssuer(Config{WSSTicketSigningSeed: bytes.Repeat([]byte{0x31}, 32)})
	issuer.now = func() time.Time { return now }

	unknown := requestWSSTicket(t, store, issuer, relay.WSSSessionTicketRequest{RelayID: desc.ID, FrontID: "front-b"})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unadvertised front status = %d", unknown.Code)
	}
	issuer.now = func() time.Time { return now.Add(time.Minute) }
	expired := requestWSSTicket(t, store, issuer, relay.WSSSessionTicketRequest{RelayID: desc.ID, FrontID: "front-a"})
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired relay status = %d", expired.Code)
	}
}

func TestWSSTicketClipsExpiryToRelayLease(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	desc := registerWSSRelayForTest(t, store, now, 30*time.Second)
	issuer := newWSSTicketIssuer(Config{
		WSSTicketSigningSeed: bytes.Repeat([]byte{0x32}, 32),
		WSSTicketTTL:         2 * time.Minute,
	})
	issuer.now = func() time.Time { return now }
	recorder := requestWSSTicket(t, store, issuer, relay.WSSSessionTicketRequest{RelayID: desc.ID, FrontID: "front-a"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response relay.WSSSessionTicketResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ExpiresAt.Equal(desc.ExpiresAt) {
		t.Fatalf("ticket expiry = %s, relay expiry = %s", response.ExpiresAt, desc.ExpiresAt)
	}
}

func TestWSSTicketRejectsUnknownJSONFields(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	issuer := newWSSTicketIssuer(Config{WSSTicketSigningSeed: bytes.Repeat([]byte{0x33}, 32)})
	issuer.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wss/tickets", strings.NewReader(
		`{"relay_id":"relay_x","front_id":"front-a","target_host":"127.0.0.1"}`,
	))
	recorder := httptest.NewRecorder()
	wssTicketHandler(NewStore(), issuer)(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestReserveWSSCandidateUsesOnlyRelayOwnedFronts(t *testing.T) {
	plain := func(id string) relay.Descriptor { return relay.Descriptor{ID: id} }
	wss := relay.Descriptor{
		ID: "wss", NodeClass: relay.NodeClassFoundation, Transport: relay.TransportDirect,
		ExitMode: relay.ExitModeDirect, PublicPort: 443, IdentityPublicKey: "identity",
		WSSFronts: []relay.WSSFrontDescriptor{{ID: "front-a", ProtocolVersion: relay.WSSProtocolVersion}},
	}
	page := reserveWSSCandidate([]relay.Descriptor{plain("a"), plain("b"), plain("c"), wss}, 3)
	if len(page) != 3 || page[0].ID != "a" || page[1].ID != "b" || page[2].ID != "wss" {
		t.Fatalf("reserved page = %+v", page)
	}
	if page[2].WSSFronts[0].ID != "front-a" {
		t.Fatal("reservation did not preserve the relay's own front")
	}
	already := reserveWSSCandidate([]relay.Descriptor{wss, plain("a"), plain("b"), plain("c")}, 3)
	if already[0].ID != "wss" || already[2].ID != "b" {
		t.Fatalf("page with WSS candidate was reordered: %+v", already)
	}
}

func registerWSSRelayForTest(t *testing.T, store *Store, now time.Time, ttl time.Duration) relay.Descriptor {
	t.Helper()
	_, identityKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	req.Transport = relay.TransportDirect
	req.PublicPort = 443
	req.ExitMode = relay.ExitModeDirect
	req.WSSFronts = []relay.WSSFrontDescriptor{{
		ID: "front-a", URL: "wss://relay-a.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion,
	}}
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt =
		relay.SignIdentity(identityKey, req, now.Add(time.Hour))
	req.WSSCapabilityProof, req.WSSCapabilityExpiresAt, err =
		relay.SignWSSCapability(identityKey, req, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	desc, err := store.Register(req, now, ttl)
	if err != nil {
		t.Fatalf("register WSS relay: %v", err)
	}
	return desc
}

func requestWSSTicket(t *testing.T, store RelayStore, issuer *wssTicketIssuer, request relay.WSSSessionTicketRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wss/tickets", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	wssTicketHandler(store, issuer)(recorder, req)
	return recorder
}
