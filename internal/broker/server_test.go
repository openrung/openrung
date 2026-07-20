package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestValidateRegisterRequest(t *testing.T) {
	req := validRegisterRequest()
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected valid request: %v", err)
	}

	req.Protocol = "unknown"
	if err := validateRegisterRequest(req); err == nil {
		t.Fatal("expected protocol validation error")
	}
}

func TestHeartbeatRelayID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wantID string
		wantOK bool
	}{
		{name: "canonical relay route", path: "/api/v1/relays/relay_abc/heartbeat", wantID: "relay_abc", wantOK: true},
		{name: "retired legacy prefix", path: "/api/v1/volunteers/relay_abc/heartbeat"},
		{name: "missing relay ID", path: "/api/v1/relays//heartbeat"},
		{name: "nested relay ID", path: "/api/v1/relays/group/relay_abc/heartbeat"},
		{name: "wrong suffix", path: "/api/v1/relays/relay_abc/pulse"},
		{name: "unrelated route", path: "/api/v1/telemetry/relay_abc/heartbeat"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, ok := heartbeatRelayID(test.path)
			if id != test.wantID || ok != test.wantOK {
				t.Fatalf("heartbeatRelayID(%q) = (%q, %v), want (%q, %v)", test.path, id, ok, test.wantID, test.wantOK)
			}
		})
	}
}

func TestCanonicalRelayRoutesRegisterAndHeartbeat(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	body, err := json.Marshal(validRegisterRequest())
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}

	registerRecorder := httptest.NewRecorder()
	server.ServeHTTP(registerRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if registerRecorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registerRecorder.Code, registerRecorder.Body.String())
	}
	if got := registerRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("register Cache-Control = %q, want no-store", got)
	}

	var desc relay.Descriptor
	if err := json.Unmarshal(registerRecorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	var registerFields map[string]any
	if err := json.Unmarshal(registerRecorder.Body.Bytes(), &registerFields); err != nil {
		t.Fatalf("decode registration fields: %v", err)
	}
	if _, ok := registerFields["lease_token"]; ok {
		t.Fatalf("legacy identityless registration unexpectedly returned lease_token: %s", registerRecorder.Body.String())
	}
	heartbeatRecorder := httptest.NewRecorder()
	heartbeatPath := "/api/v1/relays/" + desc.ID + "/heartbeat"
	server.ServeHTTP(heartbeatRecorder, httptest.NewRequest(http.MethodPost, heartbeatPath, nil))
	if heartbeatRecorder.Code != http.StatusOK {
		t.Fatalf("heartbeat: expected 200, got %d: %s", heartbeatRecorder.Code, heartbeatRecorder.Body.String())
	}
}

func TestStableRegistrationLeaseTokenRequiredAndNotListed(t *testing.T) {
	now := time.Now().UTC()
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), RelayLeaseTTL: time.Minute})
	req := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
		r.Transport = relay.TransportTunnel
	}, now)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}

	registerRecorder := httptest.NewRecorder()
	server.ServeHTTP(registerRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if registerRecorder.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registerRecorder.Code, registerRecorder.Body.String())
	}
	if got := registerRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("stable register Cache-Control = %q, want no-store", got)
	}
	var registration relay.RegisterResponse
	if err := json.Unmarshal(registerRecorder.Body.Bytes(), &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	desc := registration.Descriptor
	if desc.LeaseToken == "" {
		t.Fatal("stable registration response omitted lease_token")
	}

	heartbeatPath := "/api/v1/relays/" + desc.ID + "/heartbeat"
	missingRecorder := httptest.NewRecorder()
	server.ServeHTTP(missingRecorder, httptest.NewRequest(http.MethodPost, heartbeatPath, nil))
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing-token heartbeat: expected 404, got %d: %s", missingRecorder.Code, missingRecorder.Body.String())
	}
	heartbeatBody, err := json.Marshal(relay.HeartbeatRequest{OK: true, LeaseToken: desc.LeaseToken})
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	validRecorder := httptest.NewRecorder()
	server.ServeHTTP(validRecorder, httptest.NewRequest(http.MethodPost, heartbeatPath, bytes.NewReader(heartbeatBody)))
	if validRecorder.Code != http.StatusOK {
		t.Fatalf("valid heartbeat: expected 200, got %d: %s", validRecorder.Code, validRecorder.Body.String())
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list relays: expected 200, got %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Relays []map[string]any `json:"relays"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode relay list: %v", err)
	}
	if len(listed.Relays) != 1 {
		t.Fatalf("listed relays = %d, want 1", len(listed.Relays))
	}
	if _, ok := listed.Relays[0]["lease_token"]; ok {
		t.Fatalf("public relay list leaked lease_token: %s", listRecorder.Body.String())
	}
}

func TestRelayVersionAPIMigration(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	requestBody, err := json.Marshal(validRegisterRequest())
	if err != nil {
		t.Fatalf("marshal canonical request: %v", err)
	}

	registerRecorder := httptest.NewRecorder()
	server.ServeHTTP(registerRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(requestBody)))
	if registerRecorder.Code != http.StatusCreated {
		t.Fatalf("register canonical request: expected 201, got %d: %s", registerRecorder.Code, registerRecorder.Body.String())
	}
	var registered map[string]any
	if err := json.Unmarshal(registerRecorder.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	assertRelayVersionFields(t, registered, "test")

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list relays: expected 200, got %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Relays []map[string]any `json:"relays"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Relays) != 1 {
		t.Fatalf("listed relays = %d, want 1", len(list.Relays))
	}
	assertRelayVersionFields(t, list.Relays[0], "test")

	var legacyRequest map[string]any
	if err := json.Unmarshal(requestBody, &legacyRequest); err != nil {
		t.Fatalf("decode canonical request: %v", err)
	}
	delete(legacyRequest, "relay_version")
	legacyRequest["volunteer_version"] = "legacy-version"
	legacyBody, err := json.Marshal(legacyRequest)
	if err != nil {
		t.Fatalf("marshal legacy request: %v", err)
	}
	legacyRecorder := httptest.NewRecorder()
	NewServer(NewStore(), Config{SigningSeed: testSigningSeed()}).ServeHTTP(
		legacyRecorder,
		httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(legacyBody)),
	)
	if legacyRecorder.Code != http.StatusCreated {
		t.Fatalf("register legacy request: expected 201, got %d: %s", legacyRecorder.Code, legacyRecorder.Body.String())
	}
	var legacyRegistered map[string]any
	if err := json.Unmarshal(legacyRecorder.Body.Bytes(), &legacyRegistered); err != nil {
		t.Fatalf("decode legacy register response: %v", err)
	}
	assertRelayVersionFields(t, legacyRegistered, "legacy-version")
}

func assertRelayVersionFields(t *testing.T, fields map[string]any, want string) {
	t.Helper()
	if fields["relay_version"] != want {
		t.Fatalf("relay_version = %#v, want %q", fields["relay_version"], want)
	}
	if fields["volunteer_version"] != want {
		t.Fatalf("volunteer_version compatibility alias = %#v, want %q", fields["volunteer_version"], want)
	}
}

func TestRegisterRouteRejectsMalformedJSON(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", strings.NewReader("{")))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRetiredVolunteerWriteRoutesReturnNotFound(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	paths := []string{
		"/api/v1/volunteers/register",
		"/api/v1/volunteers/relay_abc/heartbeat",
	}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("POST %s: expected 404, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestValidateExitModes(t *testing.T) {
	req := validRegisterRequest()
	req.ExitMode = relay.ExitModeDedicated
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected dedicated exit mode to be schema-valid: %v", err)
	}
}

func TestValidateTransport(t *testing.T) {
	req := validRegisterRequest()

	req.Transport = relay.TransportTunnel
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected tunnel transport to be valid: %v", err)
	}

	req.Transport = "" // empty is allowed and treated as direct downstream
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected empty transport to be valid: %v", err)
	}

	req.Transport = "carrier-pigeon"
	if err := validateRegisterRequest(req); err == nil {
		t.Fatal("expected unknown transport to be rejected")
	}
}

func TestValidateExitHostRequiresTunnelTransport(t *testing.T) {
	req := validRegisterRequest()
	req.ExitHost = "198.51.100.7"
	if err := validateRegisterRequest(req); err == nil {
		t.Fatal("expected exit_host on direct transport to be rejected")
	}

	req.Transport = relay.TransportTunnel
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected exit_host on tunnel transport to be valid: %v", err)
	}
}

func TestRegisterDefaultsTransportDirect(t *testing.T) {
	store := NewStore()
	desc, err := store.Register(validRegisterRequest(), time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.Transport != relay.TransportDirect {
		t.Fatalf("transport = %q, want %q", desc.Transport, relay.TransportDirect)
	}
}

func TestRegisterStoresAndReturnsLabel(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	req := validRegisterRequest()
	req.Label = "happy-hippo"
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var desc relay.Descriptor
	if err := json.Unmarshal(recorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if desc.Label != "happy-hippo" {
		t.Fatalf("expected label happy-hippo, got %q", desc.Label)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if !strings.Contains(listRecorder.Body.String(), `"label":"happy-hippo"`) {
		t.Fatalf("expected label in list response: %s", listRecorder.Body.String())
	}
}

func TestRegisterRejectsUnsafeLabel(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	req := validRegisterRequest()
	req.Label = "<script>alert(1)</script>"
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe label, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	oversized := []byte(`{"public_host":"` + strings.Repeat("a", maxRegisterBodyBytes) + `"}`)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(oversized)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized register body, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterResolvesRelayLocation(t *testing.T) {
	resolver := &stubGeoResolver{geo: relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP", Latitude: 35.6895, Longitude: 139.6917}}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), GeoIP: resolver})

	body, _ := json.Marshal(validRegisterRequest())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var desc relay.Descriptor
	if err := json.Unmarshal(recorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if desc.GeoLocation != resolver.geo {
		t.Fatalf("expected register response to carry geo, got %+v", desc.GeoLocation)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	listBody := listRecorder.Body.String()
	for _, want := range []string{`"city":"Tokyo"`, `"country":"Japan"`, `"country_code":"JP"`, `"latitude":35.6895`, `"longitude":139.6917`} {
		if !strings.Contains(listBody, want) {
			t.Fatalf("expected %s in list response: %s", want, listBody)
		}
	}
}

func TestRegisterGeolocatesTunnelRelayByExitHostWithoutExposingIt(t *testing.T) {
	resolver := &stubGeoResolver{geo: relay.GeoLocation{City: "Tehran", Country: "Iran", CountryCode: "IR"}}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), GeoIP: resolver})

	req := validRegisterRequest()
	req.PublicHost = "203.0.113.1" // relay hub
	req.Transport = relay.TransportTunnel
	req.ExitHost = "198.51.100.7" // relay's observed exit IP
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(resolver.hosts) != 1 || resolver.hosts[0] != "198.51.100.7" {
		t.Fatalf("expected geo lookup against the exit host, got %v", resolver.hosts)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	listBody := listRecorder.Body.String()
	if !strings.Contains(listBody, `"city":"Tehran"`) {
		t.Fatalf("expected exit-host geo in list response: %s", listBody)
	}
	// The relay's observed exit IP must never leak through the public API.
	for _, response := range []string{recorder.Body.String(), listBody} {
		if strings.Contains(response, "198.51.100.7") {
			t.Fatalf("exit host leaked into API response: %s", response)
		}
	}
}

func TestHeartbeatBackfillsRelayLocation(t *testing.T) {
	resolver := &stubGeoResolver{err: errors.New("geoip endpoint down")}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), GeoIP: resolver})

	body, _ := json.Marshal(validRegisterRequest())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var desc relay.Descriptor
	if err := json.Unmarshal(recorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if desc.GeoLocation != (relay.GeoLocation{}) {
		t.Fatalf("expected failed lookup to leave geo empty, got %+v", desc.GeoLocation)
	}

	resolver.err = nil
	resolver.geo = relay.GeoLocation{City: "Osaka", Country: "Japan", CountryCode: "JP"}
	heartbeat := func() {
		t.Helper()
		heartbeatRecorder := httptest.NewRecorder()
		server.ServeHTTP(heartbeatRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", nil))
		if heartbeatRecorder.Code != http.StatusOK {
			t.Fatalf("expected 200 heartbeat, got %d: %s", heartbeatRecorder.Code, heartbeatRecorder.Body.String())
		}
	}
	heartbeat()

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if !strings.Contains(listRecorder.Body.String(), `"city":"Osaka"`) {
		t.Fatalf("expected heartbeat to backfill geo: %s", listRecorder.Body.String())
	}

	lookupsAfterBackfill := resolver.lookups
	heartbeat()
	if resolver.lookups != lookupsAfterBackfill {
		t.Fatalf("expected no further lookups once geo is stored, got %d after %d", resolver.lookups, lookupsAfterBackfill)
	}
}

func TestHealthzReportsStoreFailure(t *testing.T) {
	server := NewServer(failingStore{Store: NewStore(), pingErr: errors.New("database down")}, Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestListRelaysReportsStoreFailure(t *testing.T) {
	server := NewServer(failingStore{Store: NewStore(), listErr: errors.New("database down")}, Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestListRelaysIsNeverCacheable(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The signed success path adds no-transform so middleboxes on the cleartext
	// direct-IP path do not recompress the byte-exact signed body.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("expected no-store, no-transform cache control on relay list, got %q", got)
	}

	failingServer := NewServer(failingStore{Store: NewStore(), listErr: errors.New("database down")}, Config{SigningSeed: testSigningSeed()})
	failRecorder := httptest.NewRecorder()
	failingServer.ServeHTTP(failRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if got := failRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected no-store cache control on relay list error, got %q", got)
	}
}

func TestHeartbeatRouteRejectsMissingOrMalformedRelays(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "missing relay", path: "/api/v1/relays/relay_missing/heartbeat"},
		{name: "malformed endpoint", path: "/api/v1/relays/relay_missing/pulse"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

type stubGeoResolver struct {
	geo     relay.GeoLocation
	err     error
	lookups int
	hosts   []string
}

func (s *stubGeoResolver) Lookup(_ context.Context, host string) (relay.GeoLocation, error) {
	s.lookups++
	s.hosts = append(s.hosts, host)
	return s.geo, s.err
}

type failingStore struct {
	*Store
	pingErr error
	listErr error
}

func (s failingStore) Ping(context.Context) error {
	return s.pingErr
}

func (s failingStore) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(now, limit)
}
