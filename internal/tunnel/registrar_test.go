package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

const (
	canonicalRegisterPath = "/api/v1/relays/register"
	legacyRegisterPath    = "/api/v1/volunteers/register"
)

func TestBrokerRegistrarUsesCanonicalRoutes(t *testing.T) {
	t.Parallel()

	expiresAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		if got := r.Header.Get("Authorization"); got != "Bearer relay-token" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case canonicalRegisterPath:
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{
				ID:         "relay_canonical",
				PublicHost: "203.0.113.10",
				PublicPort: 443,
				ExpiresAt:  expiresAt,
			})
		case "/api/v1/relays/relay_canonical/heartbeat":
			writeRegistrarJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true, ExpiresAt: expiresAt})
		default:
			writeRegistrarJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "relay-token", server.Client())
	got, err := registrar.Register(context.Background(), testRegisterRequest())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	want := RelayRegistration{
		RelayID:    "relay_canonical",
		PublicHost: "203.0.113.10",
		PublicPort: 443,
		ExpiresAt:  expiresAt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Register() = %+v, want %+v", got, want)
	}
	if err := registrar.Heartbeat(context.Background(), got.RelayID); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		canonicalRegisterPath,
		"/api/v1/relays/relay_canonical/heartbeat",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestBrokerRegistrarRegister404FallbackIsSticky(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		switch r.URL.Path {
		case canonicalRegisterPath:
			http.NotFound(w, r) // matches an old broker's ServeMux response
		case legacyRegisterPath:
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "relay_legacy"})
		case "/api/v1/volunteers/relay_legacy/heartbeat":
			writeRegistrarJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true})
		case "/api/v1/volunteers/relay_missing/heartbeat":
			writeRegistrarJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "relay not found"})
		default:
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	for i := 0; i < 2; i++ {
		if _, err := registrar.Register(context.Background(), testRegisterRequest()); err != nil {
			t.Fatalf("Register() call %d error = %v", i+1, err)
		}
	}
	if err := registrar.Heartbeat(context.Background(), "relay_legacy"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := registrar.Heartbeat(context.Background(), "relay_missing"); !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("Heartbeat() missing relay error = %v, want ErrRelayNotFound", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		canonicalRegisterPath,
		legacyRegisterPath,
		legacyRegisterPath,
		"/api/v1/volunteers/relay_legacy/heartbeat",
		"/api/v1/volunteers/relay_missing/heartbeat",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestBrokerRegistrarHeartbeatFirst405FallbackIsSticky(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		path        string
		authorize   string
		contentType string
		body        []byte
	}
	var (
		mu       sync.Mutex
		requests []observedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, observedRequest{
			path:        r.URL.Path,
			authorize:   r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v1/relays/relay_existing/heartbeat":
			writeRegistrarJSON(w, http.StatusMethodNotAllowed, relay.ErrorResponse{Error: "method not allowed"})
		case "/api/v1/volunteers/relay_existing/heartbeat":
			writeRegistrarJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true})
		case legacyRegisterPath:
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "relay_existing"})
		default:
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "relay-token", server.Client())
	if err := registrar.Heartbeat(context.Background(), "relay_existing"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if _, err := registrar.Register(context.Background(), testRegisterRequest()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	mu.Lock()
	gotRequests := append([]observedRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 3 {
		t.Fatalf("request count = %d, want 3: %+v", len(gotRequests), gotRequests)
	}
	wantPaths := []string{
		"/api/v1/relays/relay_existing/heartbeat",
		"/api/v1/volunteers/relay_existing/heartbeat",
		legacyRegisterPath,
	}
	for i, wantPath := range wantPaths {
		if gotRequests[i].path != wantPath {
			t.Errorf("request %d path = %q, want %q", i+1, gotRequests[i].path, wantPath)
		}
		if gotRequests[i].authorize != "Bearer relay-token" {
			t.Errorf("request %d Authorization = %q, want bearer token", i+1, gotRequests[i].authorize)
		}
		if gotRequests[i].contentType != "application/json" {
			t.Errorf("request %d Content-Type = %q, want application/json", i+1, gotRequests[i].contentType)
		}
	}
	if !bytes.Equal(gotRequests[0].body, gotRequests[1].body) {
		t.Errorf("heartbeat retry body = %q, want canonical body %q", gotRequests[1].body, gotRequests[0].body)
	}
	if !bytes.Equal(gotRequests[0].body, []byte(`{"ok":true}`)) {
		t.Errorf("heartbeat body = %q, want {\"ok\":true}", gotRequests[0].body)
	}
}

func TestBrokerRegistrarFallbackPreservesRegisterRequest(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		path        string
		authorize   string
		contentType string
		body        []byte
	}
	var (
		mu       sync.Mutex
		requests []observedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, observedRequest{
			path:        r.URL.Path,
			authorize:   r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		mu.Unlock()

		if r.URL.Path == canonicalRegisterPath {
			writeRegistrarJSON(w, http.StatusMethodNotAllowed, relay.ErrorResponse{Error: "method not allowed"})
			return
		}
		if r.URL.Path == legacyRegisterPath {
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "relay_legacy"})
			return
		}
		writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
	}))
	t.Cleanup(server.Close)

	req := testRegisterRequest()
	registrar := NewBrokerRegistrar(server.URL, "relay-token", server.Client())
	if _, err := registrar.Register(context.Background(), req); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	mu.Lock()
	gotRequests := append([]observedRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("request count = %d, want 2: %+v", len(gotRequests), gotRequests)
	}
	if gotRequests[0].path != canonicalRegisterPath || gotRequests[1].path != legacyRegisterPath {
		t.Fatalf("request paths = [%q %q], want [%q %q]", gotRequests[0].path, gotRequests[1].path, canonicalRegisterPath, legacyRegisterPath)
	}
	wantBody, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal(register request) error = %v", err)
	}
	for i, got := range gotRequests {
		if got.authorize != "Bearer relay-token" {
			t.Errorf("request %d Authorization = %q, want bearer token", i+1, got.authorize)
		}
		if got.contentType != "application/json" {
			t.Errorf("request %d Content-Type = %q, want application/json", i+1, got.contentType)
		}
		if !bytes.Equal(got.body, wantBody) {
			t.Errorf("request %d body = %q, want %q", i+1, got.body, wantBody)
		}
	}
}

func TestBrokerRegistrarRelayNotFoundDoesNotFallback(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v1/relays/relay_missing/heartbeat":
			writeRegistrarJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "relay not found"})
		case canonicalRegisterPath:
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "relay_new"})
		default:
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "legacy route must not be called"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	err := registrar.Heartbeat(context.Background(), "relay_missing")
	if !errors.Is(err, ErrRelayNotFound) {
		t.Fatalf("Heartbeat() error = %v, want ErrRelayNotFound", err)
	}
	if _, err := registrar.Register(context.Background(), testRegisterRequest()); err != nil {
		t.Fatalf("Register() after relay-not-found error = %v", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		"/api/v1/relays/relay_missing/heartbeat",
		canonicalRegisterPath,
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestBrokerRegistrarDoesNotFallbackOnOtherHTTPFailures(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusNotFound,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			var canonical, legacy atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case canonicalRegisterPath:
					canonical.Add(1)
					writeRegistrarJSON(w, status, relay.ErrorResponse{Error: "deliberate failure"})
				case legacyRegisterPath:
					legacy.Add(1)
					writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "must_not_register"})
				default:
					writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
				}
			}))
			t.Cleanup(server.Close)

			registrar := NewBrokerRegistrar(server.URL, "", server.Client())
			if _, err := registrar.Register(context.Background(), testRegisterRequest()); err == nil {
				t.Fatal("Register() error = nil, want failure")
			}
			if got := canonical.Load(); got != 1 {
				t.Errorf("canonical request count = %d, want 1", got)
			}
			if got := legacy.Load(); got != 0 {
				t.Errorf("legacy request count = %d, want 0", got)
			}
		})
	}
}

func TestBrokerRegistrarDoesNotFallbackOnOther404Shapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "empty body", contentType: "text/plain; charset=utf-8"},
		{name: "HTML body", contentType: "text/html; charset=utf-8", body: "<h1>Not Found</h1>"},
		{name: "malformed JSON", contentType: "application/json", body: `{"error":`},
		{name: "other plaintext", contentType: "text/plain; charset=utf-8", body: "route not found\n"},
		{name: "legacy body with wrong type", contentType: "text/html", body: legacyServeMuxNotFoundBody},
		{name: "plaintext prefix type", contentType: "text/plain-legacy", body: legacyServeMuxNotFoundBody},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var canonical, legacy atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case canonicalRegisterPath:
					canonical.Add(1)
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, tc.body)
				case legacyRegisterPath:
					legacy.Add(1)
					writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "must_not_register"})
				default:
					writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
				}
			}))
			t.Cleanup(server.Close)

			registrar := NewBrokerRegistrar(server.URL, "", server.Client())
			if _, err := registrar.Register(context.Background(), testRegisterRequest()); err == nil {
				t.Fatal("Register() error = nil, want 404 failure")
			}
			if got := canonical.Load(); got != 1 {
				t.Errorf("canonical request count = %d, want 1", got)
			}
			if got := legacy.Load(); got != 0 {
				t.Errorf("legacy request count = %d, want 0", got)
			}
		})
	}
}

func TestBrokerRegistrarRefusesRedirectsWithoutFallback(t *testing.T) {
	t.Parallel()

	for _, redirectStatus := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		redirectStatus := redirectStatus
		t.Run(http.StatusText(redirectStatus), func(t *testing.T) {
			t.Parallel()

			var canonical, redirected, legacy atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case canonicalRegisterPath:
					canonical.Add(1)
					http.Redirect(w, r, "/redirect-target", redirectStatus)
				case "/redirect-target":
					redirected.Add(1)
					writeRegistrarJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "route not found"})
				case legacyRegisterPath:
					legacy.Add(1)
					writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "must_not_register"})
				default:
					writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
				}
			}))
			t.Cleanup(server.Close)

			registrar := NewBrokerRegistrar(server.URL, "", server.Client())
			for i := 0; i < 2; i++ {
				if _, err := registrar.Register(context.Background(), testRegisterRequest()); err == nil {
					t.Fatalf("Register() call %d error = nil, want redirected failure", i+1)
				}
			}
			if got := canonical.Load(); got != 2 {
				t.Errorf("canonical request count = %d, want 2", got)
			}
			if got := redirected.Load(); got != 0 {
				t.Errorf("redirect-target request count = %d, want 0", got)
			}
			if got := legacy.Load(); got != 0 {
				t.Errorf("legacy request count = %d, want 0", got)
			}
		})
	}
}

func TestBrokerRegistrarDoesNotFallbackOnNetworkError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Path != canonicalRegisterPath {
			t.Errorf("request path = %q, want %q", req.URL.Path, canonicalRegisterPath)
		}
		return nil, errors.New("network unavailable")
	})}
	registrar := NewBrokerRegistrar("http://broker.invalid", "", client)
	if _, err := registrar.Register(context.Background(), testRegisterRequest()); err == nil {
		t.Fatal("Register() error = nil, want network failure")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP request count = %d, want 1", got)
	}
}

func TestBrokerRegistrarDoesNotFallbackOnDecodeError(t *testing.T) {
	t.Parallel()

	var canonical, legacy atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case canonicalRegisterPath:
			canonical.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":`)
		case legacyRegisterPath:
			legacy.Add(1)
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "must_not_register"})
		default:
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	if _, err := registrar.Register(context.Background(), testRegisterRequest()); err == nil {
		t.Fatal("Register() error = nil, want decode failure")
	}
	if got := canonical.Load(); got != 1 {
		t.Errorf("canonical request count = %d, want 1", got)
	}
	if got := legacy.Load(); got != 0 {
		t.Errorf("legacy request count = %d, want 0", got)
	}
}

func TestBrokerRegistrarConcurrentLegacyDiscovery(t *testing.T) {
	t.Parallel()

	const workers = 48
	var canonical, legacy atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case canonicalRegisterPath:
			canonical.Add(1)
			http.NotFound(w, r)
		case legacyRegisterPath:
			legacy.Add(1)
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{ID: "relay_legacy"})
		default:
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := registrar.Register(context.Background(), testRegisterRequest())
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Register() error = %v", err)
		}
	}

	canonicalAfterDiscovery := canonical.Load()
	if canonicalAfterDiscovery < 1 || canonicalAfterDiscovery > workers {
		t.Errorf("canonical discovery request count = %d, want between 1 and %d", canonicalAfterDiscovery, workers)
	}
	if got := legacy.Load(); got != workers {
		t.Errorf("legacy request count = %d, want %d", got, workers)
	}

	if _, err := registrar.Register(context.Background(), testRegisterRequest()); err != nil {
		t.Fatalf("Register() after concurrent discovery error = %v", err)
	}
	if got := canonical.Load(); got != canonicalAfterDiscovery {
		t.Errorf("canonical requests after sticky fallback = %d, want %d", got, canonicalAfterDiscovery)
	}
	if got := legacy.Load(); got != workers+1 {
		t.Errorf("legacy requests after sticky fallback = %d, want %d", got, workers+1)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testRegisterRequest() relay.RegisterRequest {
	return relay.RegisterRequest{
		PublicHost:       "198.51.100.20",
		PublicPort:       8443,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         "11111111-2222-3333-4444-555555555555",
		RealityPublicKey: "test-reality-public-key",
		ShortID:          "0123456789abcdef",
		ServerName:       "www.example.com",
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		MaxSessions:      32,
		MaxMbps:          100,
		RelayVersion:     "test-version",
		Label:            "test-relay",
		NodeClass:        relay.NodeClassVolunteer,
		Transport:        relay.TransportTunnel,
		PunchCapable:     true,
		PunchEndpoint:    "https://198.51.100.20:9444",
		ExitHost:         "192.0.2.30",
	}
}

func writeRegistrarJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
