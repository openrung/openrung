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
		{name: "legacy volunteer route", path: "/api/v1/volunteers/relay_abc/heartbeat", wantID: "relay_abc", wantOK: true},
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

func TestRelayRouteAliasesRegisterAndHeartbeat(t *testing.T) {
	tests := []struct {
		name          string
		registerPath  string
		heartbeatBase string
	}{
		{name: "canonical routes", registerPath: "/api/v1/relays/register", heartbeatBase: "/api/v1/relays/"},
		{name: "canonical register legacy heartbeat", registerPath: "/api/v1/relays/register", heartbeatBase: "/api/v1/volunteers/"},
		{name: "legacy register canonical heartbeat", registerPath: "/api/v1/volunteers/register", heartbeatBase: "/api/v1/relays/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
			body, err := json.Marshal(validRegisterRequest())
			if err != nil {
				t.Fatalf("marshal register request: %v", err)
			}

			registerRecorder := httptest.NewRecorder()
			server.ServeHTTP(registerRecorder, httptest.NewRequest(http.MethodPost, test.registerPath, bytes.NewReader(body)))
			if registerRecorder.Code != http.StatusCreated {
				t.Fatalf("register: expected 201, got %d: %s", registerRecorder.Code, registerRecorder.Body.String())
			}

			var desc relay.Descriptor
			if err := json.Unmarshal(registerRecorder.Body.Bytes(), &desc); err != nil {
				t.Fatalf("decode descriptor: %v", err)
			}
			heartbeatRecorder := httptest.NewRecorder()
			heartbeatPath := test.heartbeatBase + desc.ID + "/heartbeat"
			server.ServeHTTP(heartbeatRecorder, httptest.NewRequest(http.MethodPost, heartbeatPath, nil))
			if heartbeatRecorder.Code != http.StatusOK {
				t.Fatalf("heartbeat: expected 200, got %d: %s", heartbeatRecorder.Code, heartbeatRecorder.Body.String())
			}
		})
	}
}

func TestRegisterRouteAliasesRejectMalformedJSONIdentically(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	paths := []string{
		"/api/v1/relays/register",
		"/api/v1/volunteers/register",
	}

	var wantBody string
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{")))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if wantBody == "" {
			wantBody = recorder.Body.String()
			continue
		}
		if recorder.Body.String() != wantBody {
			t.Fatalf("%s: body = %q, want %q", path, recorder.Body.String(), wantBody)
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
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
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
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe label, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	oversized := []byte(`{"public_host":"` + strings.Repeat("a", maxRegisterBodyBytes) + `"}`)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(oversized)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized register body, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterResolvesRelayLocation(t *testing.T) {
	resolver := &stubGeoResolver{geo: relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP", Latitude: 35.6895, Longitude: 139.6917}}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), GeoIP: resolver})

	body, _ := json.Marshal(validRegisterRequest())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
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
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
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
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
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
		server.ServeHTTP(heartbeatRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/"+desc.ID+"/heartbeat", nil))
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

func TestHeartbeatRoutesRejectMissingOrMalformedRelays(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "canonical missing relay", path: "/api/v1/relays/relay_missing/heartbeat"},
		{name: "legacy missing relay", path: "/api/v1/volunteers/relay_missing/heartbeat"},
		{name: "canonical malformed endpoint", path: "/api/v1/relays/relay_missing/pulse"},
		{name: "legacy malformed endpoint", path: "/api/v1/volunteers/relay_missing/pulse"},
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

func TestRelayRouteAliasesShareRegistrationRateLimit(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})

	// Exhaust the registration budget through the legacy alias. Invalid JSON
	// still consumes a token because limiting deliberately happens before body
	// parsing and authentication.
	exhausted := false
	for i := 0; i < relayRegistrationBurst+32; i++ {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", nil))
		if recorder.Code == http.StatusTooManyRequests {
			exhausted = true
			break
		}
	}
	if !exhausted {
		t.Fatal("expected legacy route to exhaust the shared budget")
	}

	// The canonical alias must immediately see that exhausted bucket. Separate
	// limiters would let callers double the effective registration budget by
	// alternating route names.
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected canonical route to share exhausted budget, got %d: %s", recorder.Code, recorder.Body.String())
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
