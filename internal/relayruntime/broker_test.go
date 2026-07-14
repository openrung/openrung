package relayruntime

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

func TestBrokerClientUsesCanonicalRoutes(t *testing.T) {
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
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		switch r.URL.Path {
		case relayRegisterPath:
			var req relay.RegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode register request: %v", err)
			}
			if !reflect.DeepEqual(req, testBrokerRegisterRequest()) {
				t.Errorf("register request = %+v, want %+v", req, testBrokerRegisterRequest())
			}
			writeBrokerJSON(w, http.StatusCreated, relay.Descriptor{
				ID:         "relay_canonical",
				PublicHost: "203.0.113.10",
				PublicPort: 443,
				ExpiresAt:  expiresAt,
			})
		case relayHeartbeatPathBase + "relay_canonical/heartbeat":
			var body map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode heartbeat request: %v", err)
			}
			if !body["ok"] {
				t.Errorf("heartbeat body = %v, want ok=true", body)
			}
			writeBrokerJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true, ExpiresAt: expiresAt})
		default:
			writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	client := &BrokerClient{BaseURL: server.URL, Token: "relay-token", HTTPClient: server.Client()}
	desc, err := client.Register(context.Background(), testBrokerRegisterRequest())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	want := relay.Descriptor{
		ID:         "relay_canonical",
		PublicHost: "203.0.113.10",
		PublicPort: 443,
		ExpiresAt:  expiresAt,
	}
	if !reflect.DeepEqual(desc, want) {
		t.Fatalf("Register() = %+v, want %+v", desc, want)
	}
	if err := client.Heartbeat(context.Background(), desc.ID); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		relayRegisterPath,
		relayHeartbeatPathBase + "relay_canonical/heartbeat",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestBrokerClientRelayNotFoundUsesCanonicalRoute(t *testing.T) {
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
		case relayHeartbeatPathBase + "relay_missing/heartbeat":
			writeBrokerJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "relay not found"})
		case relayRegisterPath:
			writeBrokerJSON(w, http.StatusCreated, relay.Descriptor{ID: "relay_new"})
		default:
			writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
		}
	}))
	t.Cleanup(server.Close)

	client := &BrokerClient{BaseURL: server.URL, HTTPClient: server.Client()}
	err := client.Heartbeat(context.Background(), "relay_missing")
	if !IsRelayNotFound(err) {
		t.Fatalf("Heartbeat() error = %v, want relay-not-found API error", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Heartbeat() error type = %T, want *APIError", err)
	}
	if apiErr.Path != relayHeartbeatPathBase+"relay_missing/heartbeat" || apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "relay not found" {
		t.Errorf("APIError = %+v, want canonical 404 relay not found", apiErr)
	}
	if _, err := client.Register(context.Background(), testBrokerRegisterRequest()); err != nil {
		t.Fatalf("Register() after relay-not-found error = %v", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		relayHeartbeatPathBase + "relay_missing/heartbeat",
		relayRegisterPath,
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestBrokerClientReturnsCanonicalHTTPFailures(t *testing.T) {
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

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != relayRegisterPath {
					writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
					return
				}
				requests.Add(1)
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "17")
				}
				writeBrokerJSON(w, status, relay.ErrorResponse{Error: "deliberate failure"})
			}))
			t.Cleanup(server.Close)

			client := &BrokerClient{BaseURL: server.URL, HTTPClient: server.Client()}
			for i := 0; i < 2; i++ {
				_, err := client.Register(context.Background(), testBrokerRegisterRequest())
				if err == nil {
					t.Fatalf("Register() call %d error = nil, want failure", i+1)
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("Register() call %d error type = %T, want *APIError", i+1, err)
				}
				if apiErr.Path != relayRegisterPath || apiErr.StatusCode != status || apiErr.Message != "deliberate failure" {
					t.Errorf("APIError = %+v, want canonical status %d", apiErr, status)
				}
				if status == http.StatusTooManyRequests && apiErr.RetryAfter != "17" {
					t.Errorf("RetryAfter = %q, want 17", apiErr.RetryAfter)
				}
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("request count = %d, want 2", got)
			}
		})
	}
}

func TestBrokerClientReturnsCanonical404Shapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "empty body", contentType: "text/plain; charset=utf-8"},
		{name: "HTML body", contentType: "text/html; charset=utf-8", body: "<h1>Not Found</h1>"},
		{name: "malformed JSON", contentType: "application/json", body: `{"error":`},
		{name: "plaintext", contentType: "text/plain; charset=utf-8", body: "404 page not found\n"},
		{name: "oversized body", contentType: "application/json", body: `{"error":"` + strings.Repeat("x", maxBrokerErrorBodyBytes)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != relayRegisterPath {
					writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			client := &BrokerClient{BaseURL: server.URL, HTTPClient: server.Client()}
			for i := 0; i < 2; i++ {
				_, err := client.Register(context.Background(), testBrokerRegisterRequest())
				if err == nil {
					t.Fatalf("Register() call %d error = nil, want 404 failure", i+1)
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("Register() call %d error type = %T, want *APIError", i+1, err)
				}
				if apiErr.Path != relayRegisterPath || apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "404 Not Found" {
					t.Errorf("APIError = %+v, want canonical 404 with status-derived message", apiErr)
				}
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("request count = %d, want 2", got)
			}
		})
	}
}

func TestBrokerClientRefusesRedirects(t *testing.T) {
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

			var requests, redirected atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case relayRegisterPath:
					requests.Add(1)
					http.Redirect(w, r, "/redirect-target", redirectStatus)
				case "/redirect-target":
					redirected.Add(1)
					writeBrokerJSON(w, http.StatusNotFound, relay.ErrorResponse{Error: "route not found"})
				default:
					writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
				}
			}))
			t.Cleanup(server.Close)

			client := &BrokerClient{BaseURL: server.URL, Token: "relay-token", HTTPClient: server.Client()}
			for i := 0; i < 2; i++ {
				_, err := client.Register(context.Background(), testBrokerRegisterRequest())
				if err == nil || !strings.Contains(err.Error(), "refused redirect") {
					t.Fatalf("Register() call %d error = %v, want redirect refusal", i+1, err)
				}
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("request count = %d, want 2", got)
			}
			if got := redirected.Load(); got != 0 {
				t.Errorf("redirect-target request count = %d, want 0", got)
			}
		})
	}
}

func TestBrokerClientReturnsNetworkOrDecodeErrors(t *testing.T) {
	t.Parallel()

	t.Run("network", func(t *testing.T) {
		var calls atomic.Int64
		httpClient := &http.Client{Transport: brokerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			if req.URL.Path != relayRegisterPath {
				t.Errorf("request path = %q, want %q", req.URL.Path, relayRegisterPath)
			}
			return nil, errors.New("network unavailable")
		})}
		client := &BrokerClient{BaseURL: "http://broker.invalid", HTTPClient: httpClient}
		for i := 0; i < 2; i++ {
			if _, err := client.Register(context.Background(), testBrokerRegisterRequest()); err == nil {
				t.Fatalf("Register() call %d error = nil, want network failure", i+1)
			}
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("HTTP request count = %d, want 2", got)
		}
	})

	t.Run("decode", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != relayRegisterPath {
				writeBrokerJSON(w, http.StatusInternalServerError, relay.ErrorResponse{Error: "unexpected path"})
				return
			}
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":`)
		}))
		t.Cleanup(server.Close)

		client := &BrokerClient{BaseURL: server.URL, HTTPClient: server.Client()}
		for i := 0; i < 2; i++ {
			if _, err := client.Register(context.Background(), testBrokerRegisterRequest()); err == nil {
				t.Fatalf("Register() call %d error = nil, want decode failure", i+1)
			}
		}
		if got := requests.Load(); got != 2 {
			t.Errorf("request count = %d, want 2", got)
		}
	})
}

func TestBrokerClientSecureTransportAllowsLoopbackHTTP(t *testing.T) {
	httpClient := &http.Client{Transport: brokerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer foundation-secret" {
			t.Errorf("Authorization = %q, want foundation bearer", got)
		}
		switch r.URL.Path {
		case relayRegisterPath:
			body, err := json.Marshal(relay.Descriptor{ID: "relay_foundation"})
			if err != nil {
				return nil, err
			}
			return brokerJSONResponse(r, http.StatusCreated, string(body)), nil
		case relayHeartbeatPathBase + "relay_foundation/heartbeat":
			return brokerJSONResponse(r, http.StatusOK, `{}`), nil
		default:
			return brokerJSONResponse(r, http.StatusNotFound, `{"error":"not found"}`), nil
		}
	})}

	client := &BrokerClient{
		BaseURL:                "http://127.0.0.1:8080",
		Token:                  "foundation-secret",
		HTTPClient:             httpClient,
		RequireSecureTransport: true,
	}
	desc, err := client.Register(context.Background(), relay.RegisterRequest{})
	if err != nil {
		t.Fatalf("Register() over loopback HTTP: %v", err)
	}
	if desc.ID != "relay_foundation" {
		t.Fatalf("Register() ID = %q, want relay_foundation", desc.ID)
	}
	if err := client.Heartbeat(context.Background(), desc.ID); err != nil {
		t.Fatalf("Heartbeat() over loopback HTTP: %v", err)
	}
}

func TestBrokerClientSecureTransportRejectsRedirectsBeforeCredentialLeak(t *testing.T) {
	operations := []struct {
		name string
		do   func(*BrokerClient) error
	}{
		{
			name: "register",
			do: func(client *BrokerClient) error {
				_, err := client.Register(context.Background(), relay.RegisterRequest{})
				return err
			},
		},
		{
			name: "heartbeat",
			do: func(client *BrokerClient) error {
				return client.Heartbeat(context.Background(), "relay_foundation")
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			var redirectedRequests atomic.Int32
			httpClient := &http.Client{Transport: brokerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Scheme == "http" {
					redirectedRequests.Add(1)
					return brokerJSONResponse(req, http.StatusOK, `{}`), nil
				}
				resp := brokerJSONResponse(req, http.StatusTemporaryRedirect, ``)
				resp.Header.Set("Location", "http://broker.test"+req.URL.Path)
				return resp, nil
			})}
			client := &BrokerClient{
				BaseURL:                "https://broker.test",
				Token:                  "foundation-secret",
				HTTPClient:             httpClient,
				RequireSecureTransport: true,
			}
			err := operation.do(client)
			if err == nil {
				t.Fatal("request error = nil, want redirect rejection")
			}
			if !strings.Contains(err.Error(), "refused redirect") {
				t.Fatalf("request error = %v, want redirect rejection", err)
			}
			if got := redirectedRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d requests, want 0; foundation credential may have leaked", got)
			}
		})
	}
}

func TestBrokerClientSecureTransportRejectsRemotePlaintextBeforeSending(t *testing.T) {
	var requests atomic.Int32
	client := &BrokerClient{
		BaseURL: "http://broker.example",
		Token:   "foundation-secret",
		HTTPClient: &http.Client{Transport: brokerRoundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, nil
		})},
		RequireSecureTransport: true,
	}

	if err := client.Heartbeat(context.Background(), "relay_foundation"); err == nil {
		t.Fatal("Heartbeat() error = nil, want plaintext rejection")
	}
	if requests.Load() != 0 {
		t.Fatalf("transport received %d requests, want 0", requests.Load())
	}
}

type brokerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f brokerRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testBrokerRegisterRequest() relay.RegisterRequest {
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
		Transport:        relay.TransportDirect,
	}
}

func writeBrokerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func brokerJSONResponse(req *http.Request, status int, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
