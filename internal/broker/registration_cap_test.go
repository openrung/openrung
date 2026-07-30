package broker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

// reserveOK reserves one unit and fails the test on refusal.
func reserveOK(t *testing.T, cap *newRelayCap, source string, now time.Time) func() {
	t.Helper()
	release, ok := cap.reserve(source, now)
	if !ok {
		t.Fatalf("reserve(%s, %s) refused", source, now)
	}
	return release
}

func TestNewRelayCap(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cap := newNewRelayCap(2, 24*time.Hour, []string{"198.51.100.0/24"})

	reserveOK(t, cap, "203.0.113.7", now)
	reserveOK(t, cap, "203.0.113.7", now.Add(20*time.Hour))
	if _, ok := cap.reserve("203.0.113.7", now.Add(20*time.Hour)); ok {
		t.Fatal("source allowed past its budget")
	}
	reserveOK(t, cap, "203.0.113.8", now.Add(20*time.Hour))

	// The window slides: 25 h in, only the first reservation has aged out, so
	// exactly one unit is free — a fixed window resetting at hour 24 would
	// wrongly hand back both and allow 2× the limit in a trailing interval.
	reserveOK(t, cap, "203.0.113.7", now.Add(25*time.Hour))
	if _, ok := cap.reserve("203.0.113.7", now.Add(25*time.Hour)); ok {
		t.Fatal("sliding window handed back a unit still inside the trailing 24 h")
	}

	// A released unit (failed registration) returns to the budget.
	release := reserveOK(t, cap, "203.0.113.9", now)
	if _, ok := cap.reserve("203.0.113.9", now); !ok {
		t.Fatal("second unit refused under limit")
	}
	if _, ok := cap.reserve("203.0.113.9", now); ok {
		t.Fatal("budget not exhausted")
	}
	release()
	release() // idempotent
	if _, ok := cap.reserve("203.0.113.9", now); !ok {
		t.Fatal("released unit was not returned to the budget")
	}

	// Exempt CIDRs are never counted or capped.
	for i := 0; i < 10; i++ {
		reserveOK(t, cap, "198.51.100.42", now)
	}

	// IPv6 sources share one bucket per /64 — a single host holds a whole /64.
	reserveOK(t, cap, "2001:db8:1:2::1", now)
	reserveOK(t, cap, "2001:db8:1:2::ffff", now)
	if _, ok := cap.reserve("2001:db8:1:2:dead:beef::1", now); ok {
		t.Fatal("IPv6 rotation within one /64 escaped the cap")
	}
	reserveOK(t, cap, "2001:db8:1:3::1", now)

	// Zero picks the default; negative disables entirely.
	if got := newNewRelayCap(0, 24*time.Hour, nil).limit; got != defaultMaxNewRelayIDsPerDay {
		t.Fatalf("default limit = %d, want %d", got, defaultMaxNewRelayIDsPerDay)
	}
	off := newNewRelayCap(-1, 24*time.Hour, nil)
	for i := 0; i < 3; i++ {
		reserveOK(t, off, "203.0.113.7", now)
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

// TestRegisterCapFoundationDoesNotConsumeAnonymousBudget pins the other half
// of the foundation bypass: a foundation registration must not spend the
// address's anonymous budget either, or one foundation relay would 429 the
// volunteer registering from the same NAT.
func TestRegisterCapFoundationDoesNotConsumeAnonymousBudget(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:               testSigningSeed(),
		FoundationToken:           "foundation-token",
		MaxNewRelayIDsPerIPPerDay: 1,
	})

	foundation := registerVia(t, server, func(r *relay.RegisterRequest) { r.PublicPort = 40100 }, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer foundation-token")
	})
	if foundation.Code != http.StatusCreated {
		t.Fatalf("foundation registration: expected 201, got %d: %s", foundation.Code, foundation.Body.String())
	}

	if recorder := registerVia(t, server, nil, nil); recorder.Code != http.StatusCreated {
		t.Fatalf("anonymous registration after foundation: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := registerVia(t, server, func(r *relay.RegisterRequest) { r.PublicPort = 40101 }, nil); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second anonymous registration: expected 429, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// TestRegisterCapChargesConcurrentSameIdentityOnce pins the budget cost of a
// first registration that races itself: `limit` simultaneous registrations
// proving the SAME previously unseen key all reserve a unit (the newIdentity
// check runs before the store call), but they mint one relay between them —
// the commit-race losers must hand their units back, or one identity would
// drain the address's whole budget and 429 the next distinct relay.
func TestRegisterCapChargesConcurrentSameIdentityOnce(t *testing.T) {
	const limit = 4
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), MaxNewRelayIDsPerIPPerDay: limit})
	priv := testIdentityKey(t)

	// Exactly `limit` concurrent duplicates: every reservation fits, so each
	// request must come back 201 regardless of commit order.
	bodies := make([][]byte, limit)
	for i := range bodies {
		request := validRegisterRequest()
		withIdentity(priv)(&request)
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal register request: %v", err)
		}
		bodies[i] = body
	}
	var created atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
			if recorder.Code == http.StatusCreated {
				created.Add(1)
			}
		}(bodies[i])
	}
	wg.Wait()
	if got := created.Load(); got != limit {
		t.Fatalf("same-identity duplicates: %d of %d got 201", got, limit)
	}

	// The race must have consumed exactly one unit: limit-1 further distinct
	// identities fit, and the one after that is over budget.
	for i := 0; i < limit-1; i++ {
		recorder := registerVia(t, server, func(r *relay.RegisterRequest) { r.PublicPort = 41000 + i }, nil)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("distinct identity %d after duplicate race: expected 201, got %d: %s", i, recorder.Code, recorder.Body.String())
		}
	}
	recorder := registerVia(t, server, func(r *relay.RegisterRequest) { r.PublicPort = 41100 }, nil)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("registration past the budget: expected 429, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// TestRegisterCapHoldsUnderConcurrency pins the atomicity of the budget: N
// simultaneous registrations from one source must not finish with more than
// the limit created (a check-then-record scheme let all of them pass the
// check first and blow through the cap).
func TestRegisterCapHoldsUnderConcurrency(t *testing.T) {
	const limit = 4
	const attempts = 80
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), MaxNewRelayIDsPerIPPerDay: limit})

	bodies := make([][]byte, attempts)
	for i := range bodies {
		request := validRegisterRequest()
		request.PublicPort = 30000 + i
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal register request: %v", err)
		}
		bodies[i] = body
	}

	var created atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
			if recorder.Code == http.StatusCreated {
				created.Add(1)
			}
		}(bodies[i])
	}
	wg.Wait()

	if got := created.Load(); got != limit {
		t.Fatalf("concurrent registrations created %d relays, want exactly %d", got, limit)
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
		"junk server_name":                 func(r *relay.RegisterRequest) { r.ServerName = "not a hostname\n" },
		"NUL in client_id":                 func(r *relay.RegisterRequest) { r.ClientID = "abc\x00def" },
		"C0 control char in relay_version": func(r *relay.RegisterRequest) { r.RelayVersion = "v1.0\x1b[31m" },
		"C1 control char in relay_version": func(r *relay.RegisterRequest) { r.RelayVersion = "v1.0\u009b31m" },
		"oversized short_id":               func(r *relay.RegisterRequest) { r.ShortID = strings.Repeat("a", 200) },
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
