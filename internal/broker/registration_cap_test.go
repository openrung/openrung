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
)

func TestNewRelayCap(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cap := newNewRelayCap(2, 24*time.Hour, []string{"198.51.100.0/24"})

	if !cap.allows("203.0.113.7", now) {
		t.Fatal("fresh source refused")
	}
	cap.record("203.0.113.7", now)
	cap.record("203.0.113.7", now)
	if cap.allows("203.0.113.7", now) {
		t.Fatal("source allowed past its budget")
	}
	if !cap.allows("203.0.113.8", now) {
		t.Fatal("budget leaked across sources")
	}

	// The window is fixed from the bucket's first entry; once it passes, the
	// budget refills.
	if !cap.allows("203.0.113.7", now.Add(24*time.Hour)) {
		t.Fatal("budget did not refill after the window")
	}

	// Exempt CIDRs are never counted or capped.
	for i := 0; i < 10; i++ {
		if !cap.allows("198.51.100.42", now) {
			t.Fatal("exempt source refused")
		}
		cap.record("198.51.100.42", now)
	}

	// IPv6 sources share one bucket per /64 — a single host holds a whole /64.
	cap.record("2001:db8:1:2::1", now)
	cap.record("2001:db8:1:2::ffff", now)
	if cap.allows("2001:db8:1:2:dead:beef::1", now) {
		t.Fatal("IPv6 rotation within one /64 escaped the cap")
	}
	if !cap.allows("2001:db8:1:3::1", now) {
		t.Fatal("distinct /64 was capped by a neighbour")
	}

	// Zero picks the default; negative disables entirely.
	if got := newNewRelayCap(0, 24*time.Hour, nil).limit; got != defaultMaxNewRelayIDsPerDay {
		t.Fatalf("default limit = %d, want %d", got, defaultMaxNewRelayIDsPerDay)
	}
	off := newNewRelayCap(-1, 24*time.Hour, nil)
	for i := 0; i < 3; i++ {
		off.record("203.0.113.7", now)
	}
	if !off.allows("203.0.113.7", now) {
		t.Fatal("disabled cap still capped")
	}
}

// registerVia posts one registration to the server, optionally reshaping the
// body and the request. Every httptest request shares the default RemoteAddr,
// so repeated calls model one source IP.
func registerVia(t *testing.T, server http.Handler, mutateBody func(*relay.RegisterRequest), mutateHTTP func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := validRegisterRequest()
	if mutateBody != nil {
		mutateBody(&request)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body))
	if mutateHTTP != nil {
		mutateHTTP(req)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func testIdentityKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv, err := relay.ParseIdentitySeed(identityStoreSeedA)
	if err != nil {
		t.Fatalf("parse identity seed: %v", err)
	}
	return priv
}

// withIdentity signs the (possibly mutated) request with a stable identity so
// its derived relay ID is the same on every call.
func withIdentity(priv ed25519.PrivateKey) func(*relay.RegisterRequest) {
	return func(req *relay.RegisterRequest) {
		req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt =
			relay.SignIdentity(priv, *req, time.Now().Add(relay.IdentityProofTTLDirect))
	}
}

func TestRegisterCapsNewIdentitiesPerSource(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), MaxNewRelayIDsPerIPPerDay: 1})

	if recorder := registerVia(t, server, nil, nil); recorder.Code != http.StatusCreated {
		t.Fatalf("first registration: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	second := registerVia(t, server, nil, nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second new identity: expected 429, got %d: %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("cap 429 must carry Retry-After so the engine backs off sanely")
	}
}

func TestRegisterCapSparesKnownIdentitiesAndFoundation(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:               testSigningSeed(),
		FoundationToken:           "foundation-token",
		MaxNewRelayIDsPerIPPerDay: 1,
	})

	// Consume the source's whole budget with one stable-identity registration.
	priv := testIdentityKey(t)
	first := registerVia(t, server, withIdentity(priv), nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("identity registration: expected 201, got %d: %s", first.Code, first.Body.String())
	}

	// Re-proving the same identity is a reconnect, not a new relay: it must
	// pass however often it happens (tunnel relays re-register constantly).
	for i := 0; i < 3; i++ {
		if recorder := registerVia(t, server, withIdentity(priv), nil); recorder.Code != http.StatusCreated {
			t.Fatalf("identity re-registration %d: expected 201, got %d: %s", i, recorder.Code, recorder.Body.String())
		}
	}

	// A different new identity from the same source is over budget.
	if recorder := registerVia(t, server, nil, nil); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("legacy registration over budget: expected 429, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// The foundation credential is operator infrastructure and bypasses the cap.
	foundation := registerVia(t, server, nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer foundation-token")
	})
	if foundation.Code != http.StatusCreated {
		t.Fatalf("foundation registration: expected 201, got %d: %s", foundation.Code, foundation.Body.String())
	}
}

func TestRegisterCapExemptCIDRAndFailedAttempts(t *testing.T) {
	// Rejected requests must not consume budget: a relay crash-looping on a
	// validation error may retry far past any cap before its operator fixes it.
	unexempt := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), MaxNewRelayIDsPerIPPerDay: 1})
	for i := 0; i < 3; i++ {
		recorder := registerVia(t, unexempt, func(r *relay.RegisterRequest) { r.Protocol = "bogus" }, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", recorder.Code)
		}
	}
	if recorder := registerVia(t, unexempt, nil, nil); recorder.Code != http.StatusCreated {
		t.Fatalf("failed attempts burned the budget: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// An exempt CIDR registers new identities without limit.
	exempt := NewServer(NewStore(), Config{
		SigningSeed:                testSigningSeed(),
		MaxNewRelayIDsPerIPPerDay:  1,
		RegistrationCapExemptCIDRs: []string{"192.0.2.0/24"}, // httptest's default RemoteAddr network
	})
	for i := 0; i < 5; i++ {
		recorder := registerVia(t, exempt, func(r *relay.RegisterRequest) { r.PublicPort = 40000 + i }, nil)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("exempt registration %d: expected 201, got %d: %s", i, recorder.Code, recorder.Body.String())
		}
	}
}

func TestValidateRegisterRequestFieldHardening(t *testing.T) {
	mutations := map[string]func(*relay.RegisterRequest){
		"junk public_host": func(r *relay.RegisterRequest) { r.PublicHost = "Free VPN! Click here" },
		"oversized public_host": func(r *relay.RegisterRequest) {
			r.PublicHost = strings.Repeat("a", 60) + "." + strings.Repeat("b", 200) + ".com"
		},
		"junk server_name":              func(r *relay.RegisterRequest) { r.ServerName = "not a hostname\n" },
		"NUL in client_id":              func(r *relay.RegisterRequest) { r.ClientID = "abc\x00def" },
		"control char in relay_version": func(r *relay.RegisterRequest) { r.RelayVersion = "v1.0\x1b[31m" },
		"oversized short_id":            func(r *relay.RegisterRequest) { r.ShortID = strings.Repeat("a", 200) },
		"junk tunnel exit_host": func(r *relay.RegisterRequest) {
			r.Transport = relay.TransportTunnel
			r.ExitHost = "definitely not\ta host"
		},
	}
	for name, mutate := range mutations {
		request := validRegisterRequest()
		mutate(&request)
		if err := validateRegisterRequest(request); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	// Legitimate shapes must all still register: IPv4 and IPv6 literals and
	// DNS hostnames are what the live fleet advertises today.
	hosts := []string{"203.0.113.9", "2406:da14:16a4:8400::1", "relay.example.com", "localhost"}
	for _, host := range hosts {
		request := validRegisterRequest()
		request.PublicHost = host
		if err := validateRegisterRequest(request); err != nil {
			t.Errorf("host %q rejected: %v", host, err)
		}
	}
}
