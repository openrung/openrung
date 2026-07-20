package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func postRegister(t *testing.T, server http.Handler, req relay.RegisterRequest, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body))
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httpReq)
	return recorder
}

func decodeDescriptor(t *testing.T, recorder *httptest.ResponseRecorder) relay.Descriptor {
	t.Helper()
	var desc relay.Descriptor
	if err := json.Unmarshal(recorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v (body: %s)", err, recorder.Body.String())
	}
	return desc
}

func TestRegisterDefaultsNodeClassVolunteer(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	recorder := postRegister(t, server, validRegisterRequest(), "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if desc := decodeDescriptor(t, recorder); desc.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("node_class = %q, want %q", desc.NodeClass, relay.NodeClassVolunteer)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if !strings.Contains(listRecorder.Body.String(), `"node_class":"volunteer"`) {
		t.Fatalf("expected node_class in signed list response: %s", listRecorder.Body.String())
	}
}

func TestRegisterPreservesNodeClassAuthorization(t *testing.T) {
	tests := []struct {
		name          string
		bearer        string
		claimClass    string
		wantStatus    int
		wantNodeClass string
	}{
		{
			name:          "volunteer credential registers volunteer relay",
			bearer:        "volunteer-token",
			wantStatus:    http.StatusCreated,
			wantNodeClass: relay.NodeClassVolunteer,
		},
		{
			name:          "foundation credential registers foundation relay",
			bearer:        "foundation-token",
			claimClass:    relay.NodeClassFoundation,
			wantStatus:    http.StatusCreated,
			wantNodeClass: relay.NodeClassFoundation,
		},
		{
			name:       "volunteer credential cannot claim foundation relay",
			bearer:     "volunteer-token",
			claimClass: relay.NodeClassFoundation,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "invalid credential is rejected",
			bearer:     "wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(NewStore(), Config{
				SigningSeed:       testSigningSeed(),
				RegistrationToken: "volunteer-token",
				FoundationToken:   "foundation-token",
			})
			req := validRegisterRequest()
			req.NodeClass = test.claimClass
			recorder := postRegister(t, server, req, test.bearer)
			if recorder.Code != test.wantStatus {
				t.Fatalf("expected %d, got %d: %s", test.wantStatus, recorder.Code, recorder.Body.String())
			}
			if test.wantStatus == http.StatusCreated {
				if desc := decodeDescriptor(t, recorder); desc.NodeClass != test.wantNodeClass {
					t.Fatalf("node_class = %q, want %q", desc.NodeClass, test.wantNodeClass)
				}
			}
		})
	}
}

func TestRegisterRejectsUnknownNodeClass(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	req := validRegisterRequest()
	req.NodeClass = "partner"
	recorder := postRegister(t, server, req, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown node_class, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// An open (anonymous) broker must still refuse foundation claims: anonymity
// authorizes volunteer-class registration only.
func TestRegisterForbidsFoundationClassAnonymously(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for anonymous foundation claim, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterForbidsFoundationClassWithVolunteerToken(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "volunteer-token")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for volunteer-token foundation claim, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterAcceptsFoundationClassWithFoundationToken(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if desc := decodeDescriptor(t, recorder); desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want %q", desc.NodeClass, relay.NodeClassFoundation)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if !strings.Contains(listRecorder.Body.String(), `"node_class":"foundation"`) {
		t.Fatalf("expected foundation node_class in signed list response: %s", listRecorder.Body.String())
	}
}

// The foundation token bounds the class a request may claim; it does not force
// it. A privileged holder can still register a volunteer-class relay, though
// routine volunteer-class relay and hub traffic should use the volunteer token.
func TestRegisterFoundationTokenDefaultsToVolunteerClass(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	recorder := postRegister(t, server, validRegisterRequest(), "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if desc := decodeDescriptor(t, recorder); desc.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("node_class = %q, want %q", desc.NodeClass, relay.NodeClassVolunteer)
	}
}

// A foundation claim with a lowercase/whitespace variant must normalize before
// the authorization check, not sneak past it.
func TestRegisterForbidsFoundationClassVariantSpelling(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = " Foundation "
	recorder := postRegister(t, server, req, "volunteer-token")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for foundation claim variant, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHeartbeatAcceptsFoundationToken(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	desc := decodeDescriptor(t, recorder)

	heartbeat := httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", nil)
	heartbeat.Header.Set("Authorization", "Bearer foundation-token")
	heartbeatRecorder := httptest.NewRecorder()
	server.ServeHTTP(heartbeatRecorder, heartbeat)
	if heartbeatRecorder.Code != http.StatusOK {
		t.Fatalf("heartbeat: expected 200, got %d: %s", heartbeatRecorder.Code, heartbeatRecorder.Body.String())
	}
}

func TestHeartbeatEnforcesFoundationToken(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		RegistrationToken: "volunteer-token",
		FoundationToken:   "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	desc := decodeDescriptor(t, recorder)
	heartbeatPath := "/api/v1/relays/" + desc.ID + "/heartbeat"

	volunteerHeartbeat := httptest.NewRequest(http.MethodPost, heartbeatPath, nil)
	volunteerHeartbeat.Header.Set("Authorization", "Bearer volunteer-token")
	volunteerRecorder := httptest.NewRecorder()
	server.ServeHTTP(volunteerRecorder, volunteerHeartbeat)
	if volunteerRecorder.Code != http.StatusForbidden {
		t.Fatalf("volunteer heartbeat: expected 403, got %d: %s", volunteerRecorder.Code, volunteerRecorder.Body.String())
	}

	foundationHeartbeat := httptest.NewRequest(http.MethodPost, heartbeatPath, nil)
	foundationHeartbeat.Header.Set("Authorization", "Bearer foundation-token")
	foundationRecorder := httptest.NewRecorder()
	server.ServeHTTP(foundationRecorder, foundationHeartbeat)
	if foundationRecorder.Code != http.StatusOK {
		t.Fatalf("foundation heartbeat: expected 200, got %d: %s", foundationRecorder.Code, foundationRecorder.Body.String())
	}
}

func TestCredentialNodeClass(t *testing.T) {
	withAuth := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", nil)
		if v != "" {
			r.Header.Set("Authorization", v)
		}
		return r
	}

	cases := []struct {
		name      string
		cfg       Config
		auth      string
		wantClass string
		wantOK    bool
	}{
		{name: "anonymous broker no header", cfg: Config{}, auth: "", wantClass: relay.NodeClassVolunteer, wantOK: true},
		{name: "anonymous broker foundation token", cfg: Config{FoundationToken: "fnd"}, auth: "Bearer fnd", wantClass: relay.NodeClassFoundation, wantOK: true},
		{name: "anonymous broker junk header", cfg: Config{FoundationToken: "fnd"}, auth: "Bearer junk", wantClass: relay.NodeClassVolunteer, wantOK: true},
		{name: "closed broker volunteer token", cfg: Config{RegistrationToken: "vol", FoundationToken: "fnd"}, auth: "Bearer vol", wantClass: relay.NodeClassVolunteer, wantOK: true},
		{name: "closed broker foundation token", cfg: Config{RegistrationToken: "vol", FoundationToken: "fnd"}, auth: "Bearer fnd", wantClass: relay.NodeClassFoundation, wantOK: true},
		{name: "closed broker wrong token", cfg: Config{RegistrationToken: "vol", FoundationToken: "fnd"}, auth: "Bearer nope", wantOK: false},
		{name: "closed broker no header", cfg: Config{RegistrationToken: "vol"}, auth: "", wantOK: false},
		// A configured-but-empty foundation token must never grant foundation
		// class to a request with no Authorization header.
		{name: "empty foundation token no header", cfg: Config{}, auth: "", wantClass: relay.NodeClassVolunteer, wantOK: true},
	}
	for _, tc := range cases {
		class, ok := credentialNodeClass(withAuth(tc.auth), tc.cfg)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok && class != tc.wantClass {
			t.Errorf("%s: class = %q, want %q", tc.name, class, tc.wantClass)
		}
	}
}

func TestStoreRegisterPreservesNodeClass(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()

	defaulted, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if defaulted.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("default node_class = %q, want %q", defaulted.NodeClass, relay.NodeClassVolunteer)
	}

	req := validRegisterRequest()
	req.PublicHost = "2001:db8::99"
	req.NodeClass = relay.NodeClassFoundation
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want %q", desc.NodeClass, relay.NodeClassFoundation)
	}

	listed, err := store.List(now, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	classes := map[string]string{}
	for _, item := range listed {
		classes[item.ID] = item.NodeClass
	}
	if classes[desc.ID] != relay.NodeClassFoundation || classes[defaulted.ID] != relay.NodeClassVolunteer {
		t.Fatalf("listed classes = %v", classes)
	}
}

// The heartbeat lease guard: a foundation relay's lease may only be extended
// by a foundation credential. This is what makes a foundation label expire
// (within one TTL) when its endpoint is taken over via a writer that predates
// node_class, e.g. a rolled-back broker binary's upsert.
func TestHeartbeatForbiddenForFoundationRelayWithoutFoundationCredential(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		FoundationToken: "foundation-token",
	})

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	recorder := postRegister(t, server, req, "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	desc := decodeDescriptor(t, recorder)

	// Anonymous heartbeat (valid volunteer-class credential on this open broker)
	// must not extend a foundation relay's lease.
	anonymous := httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", nil)
	anonymousRecorder := httptest.NewRecorder()
	server.ServeHTTP(anonymousRecorder, anonymous)
	if anonymousRecorder.Code != http.StatusForbidden {
		t.Fatalf("anonymous heartbeat: expected 403, got %d: %s", anonymousRecorder.Code, anonymousRecorder.Body.String())
	}

	authorized := httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", nil)
	authorized.Header.Set("Authorization", "Bearer foundation-token")
	authorizedRecorder := httptest.NewRecorder()
	server.ServeHTTP(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusOK {
		t.Fatalf("foundation heartbeat: expected 200, got %d: %s", authorizedRecorder.Code, authorizedRecorder.Body.String())
	}
}

func TestStoreHeartbeatGuardsFoundationLease(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	desc, err := store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := store.Heartbeat(desc.ID, desc.LeaseToken, relay.NodeClassVolunteer, now.Add(time.Second), time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
		t.Fatalf("volunteer-class credential heartbeat of foundation relay: err = %v, want ErrNodeClassForbidden", err)
	}
	// The refused heartbeat must not have extended the lease.
	if listed, err := store.List(now.Add(2*time.Minute), 10); err != nil || len(listed) != 0 {
		t.Fatalf("foundation relay lease was extended by refused heartbeat: %v %v", listed, err)
	}

	desc, err = store.Register(req, now, time.Minute)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if _, err := store.Heartbeat(desc.ID, desc.LeaseToken, relay.NodeClassFoundation, now.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("foundation-credential heartbeat: %v", err)
	}

	if _, err := store.Heartbeat("relay_missing", "", relay.NodeClassFoundation, now, time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("missing relay: err = %v, want ErrRelayNotFound", err)
	}
}

// The registration overwrite guard: a live foundation endpoint may not be
// seized by a non-foundation registration at the same public_host:public_port.
// This is the vector the heartbeat guard does NOT cover — the damage would
// otherwise happen through registration, not heartbeat.
func TestRegisterCannotOverwriteFoundationEndpointAnonymously(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		FoundationToken: "foundation-token",
	})

	foundation := validRegisterRequest()
	foundation.NodeClass = relay.NodeClassFoundation
	foundation.RealityPublicKey = "foundation-key"
	recorder := postRegister(t, server, foundation, "foundation-token")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register foundation: expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	original := decodeDescriptor(t, recorder)

	// Attacker registers the same host:port anonymously as a volunteer-class relay.
	attacker := validRegisterRequest() // same public_host:public_port
	attacker.RealityPublicKey = "attacker-key"
	attackRecorder := postRegister(t, server, attacker, "")
	if attackRecorder.Code != http.StatusForbidden {
		t.Fatalf("attacker overwrite: expected 403, got %d: %s", attackRecorder.Code, attackRecorder.Body.String())
	}

	// The signed list must still carry the original foundation descriptor,
	// unmodified — same class, same reality key, and no attacker entry.
	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	body := listRecorder.Body.String()
	if !strings.Contains(body, `"node_class":"foundation"`) || !strings.Contains(body, "foundation-key") {
		t.Fatalf("foundation descriptor was disturbed: %s", body)
	}
	if strings.Contains(body, "attacker-key") {
		t.Fatalf("attacker descriptor leaked into the list: %s", body)
	}
	_ = original
}

// An authorized foundation registration replaces both a volunteer-class relay
// that pre-squatted the endpoint and an older foundation descriptor. The
// in-memory store must retain the same one-row-per-endpoint invariant as postgres.
func TestRegisterFoundationCanRefreshOwnEndpoint(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()

	preSquat, err := store.Register(validRegisterRequest(), now, time.Minute)
	if err != nil {
		t.Fatalf("register pre-squatting volunteer-class relay: %v", err)
	}

	req := validRegisterRequest()
	req.NodeClass = relay.NodeClassFoundation
	firstFoundation, err := store.Register(req, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("first foundation register: %v", err)
	}
	if _, err := store.Heartbeat(preSquat.ID, preSquat.LeaseToken, relay.NodeClassVolunteer, now.Add(2*time.Second), time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("pre-squatting volunteer-class relay ID survived foundation registration: %v", err)
	}

	// Re-register at the same endpoint, still foundation-class: allowed.
	refreshed, err := store.Register(req, now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("foundation refresh: %v", err)
	}
	if refreshed.ID == firstFoundation.ID {
		t.Fatal("expected foundation refresh to receive a new relay ID")
	}
	if _, err := store.Heartbeat(firstFoundation.ID, firstFoundation.LeaseToken, relay.NodeClassFoundation, now.Add(3*time.Second), time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("old foundation ID survived refresh: %v", err)
	}
	listed, err := store.List(now.Add(3*time.Second), 10)
	if err != nil {
		t.Fatalf("list refreshed foundation relay: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != refreshed.ID || listed[0].NodeClass != relay.NodeClassFoundation {
		t.Fatalf("expected exactly the refreshed foundation descriptor, got %+v", listed)
	}

	// A volunteer-class relay registration at the same endpoint is refused.
	vol := validRegisterRequest()
	if _, err := store.Register(vol, now.Add(4*time.Second), time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
		t.Fatalf("volunteer-class relay collision: err = %v, want ErrNodeClassForbidden", err)
	}
}

func TestStoreExpiredFoundationEndpointIsReclaimable(t *testing.T) {
	store := NewStore()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	foundation := validRegisterRequest()
	foundation.NodeClass = relay.NodeClassFoundation
	expired, err := store.Register(foundation, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}

	volunteerClassRelay := validRegisterRequest()
	reclaimed, err := store.Register(volunteerClassRelay, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("reclaim expired foundation endpoint: %v", err)
	}
	if reclaimed.NodeClass != relay.NodeClassVolunteer {
		t.Fatalf("reclaimed node_class = %q, want %q", reclaimed.NodeClass, relay.NodeClassVolunteer)
	}
	if _, err := store.Heartbeat(expired.ID, expired.LeaseToken, relay.NodeClassFoundation, now.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("expired foundation ID survived endpoint reclaim: %v", err)
	}
	listed, err := store.List(now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatalf("list reclaimed endpoint: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != reclaimed.ID {
		t.Fatalf("expected exactly the volunteer-class relay replacement, got %+v", listed)
	}
}
