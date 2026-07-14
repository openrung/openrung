package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

const canonicalRegisterPath = "/api/v1/relays/register"

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
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		switch r.URL.Path {
		case canonicalRegisterPath:
			var req relay.RegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode register request: %v", err)
			}
			if !reflect.DeepEqual(req, testRegisterRequest()) {
				t.Errorf("register request = %+v, want %+v", req, testRegisterRequest())
			}
			writeRegistrarJSON(w, http.StatusOK, relay.Descriptor{
				ID:         "relay_canonical",
				PublicHost: "203.0.113.10",
				PublicPort: 443,
				ExpiresAt:  expiresAt,
			})
		case "/api/v1/relays/relay_canonical/heartbeat":
			var body map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode heartbeat request: %v", err)
			}
			if !body["ok"] {
				t.Errorf("heartbeat body = %v, want ok=true", body)
			}
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

func TestBrokerRegistrarMapsRelayNotFound(t *testing.T) {
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
			writeRegistrarJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
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

func TestBrokerRegistrarReturnsCanonicalHTTPFailures(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
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

			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != canonicalRegisterPath {
					t.Errorf("request path = %q, want %q", r.URL.Path, canonicalRegisterPath)
				}
				writeRegistrarJSON(w, status, relay.ErrorResponse{Error: "deliberate failure"})
			}))
			t.Cleanup(server.Close)

			registrar := NewBrokerRegistrar(server.URL, "", server.Client())
			_, err := registrar.Register(context.Background(), testRegisterRequest())
			if err == nil {
				t.Fatal("Register() error = nil, want failure")
			}
			var responseErr *brokerHTTPError
			if !errors.As(err, &responseErr) {
				t.Fatalf("Register() error type = %T, want *brokerHTTPError", err)
			}
			if responseErr.path != canonicalRegisterPath || responseErr.statusCode != status || responseErr.message != "deliberate failure" {
				t.Errorf("brokerHTTPError = %+v, want canonical status %d", responseErr, status)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("request count = %d, want 1", got)
			}
		})
	}
}

func TestBrokerRegistrarBoundsErrorBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"`+strings.Repeat("x", maxBrokerErrorBodyBytes)+`"}`)
	}))
	t.Cleanup(server.Close)

	registrar := NewBrokerRegistrar(server.URL, "", server.Client())
	_, err := registrar.Register(context.Background(), testRegisterRequest())
	var responseErr *brokerHTTPError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Register() error = %v, want *brokerHTTPError", err)
	}
	if responseErr.message != "502 Bad Gateway" {
		t.Fatalf("error message = %q, want bounded status fallback", responseErr.message)
	}
}

func TestBrokerRegistrarRefusesRedirects(t *testing.T) {
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

			var canonical, redirected atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case canonicalRegisterPath:
					canonical.Add(1)
					http.Redirect(w, r, "/redirect-target", redirectStatus)
				case "/redirect-target":
					redirected.Add(1)
					writeRegistrarJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "route not found"})
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
		})
	}
}

func TestBrokerRegistrarReturnsNetworkError(t *testing.T) {
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

func TestBrokerRegistrarReturnsDecodeError(t *testing.T) {
	t.Parallel()

	var canonical atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case canonicalRegisterPath:
			canonical.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":`)
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
