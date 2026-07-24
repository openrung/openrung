package clienttelemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    r,
	}
}

func TestTelemetryURL(t *testing.T) {
	cases := map[string]string{
		"https://broker.example.com":       "https://broker.example.com/api/v1/telemetry/events",
		"https://broker.example.com/base/": "https://broker.example.com/base/api/v1/telemetry/events",
	}
	for base, want := range cases {
		got, err := TelemetryURL(base)
		if err != nil {
			t.Fatalf("TelemetryURL(%q): %v", base, err)
		}
		if got != want {
			t.Fatalf("TelemetryURL(%q) = %q, want %q", base, got, want)
		}
	}

	if _, err := TelemetryURL("not-a-url"); err == nil {
		t.Fatal("expected error for URL without scheme/host")
	}
}

func TestSendSetsIdentityHeadersAndBody(t *testing.T) {
	var gotPath, gotClientID, gotSessionID string
	var gotBatch batch
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotClientID = r.Header.Get("X-OpenRung-Client-ID")
		gotSessionID = r.Header.Get("X-OpenRung-Session-ID")
		if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		return jsonResponse(r, http.StatusAccepted, `{"accepted":1}`), nil
	})}

	client := HTTPClient{BaseURL: "https://broker.example.com", HTTP: httpClient}
	events := []Event{{EventID: "e1", ClientID: "client-1", SessionID: "session-1", Event: "x"}}
	if err := client.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotPath != "/api/v1/telemetry/events" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotClientID != "client-1" || gotSessionID != "session-1" {
		t.Fatalf("missing identity headers: client=%q session=%q", gotClientID, gotSessionID)
	}
	if len(gotBatch.Events) != 1 || gotBatch.Events[0].EventID != "e1" {
		t.Fatalf("unexpected batch body: %+v", gotBatch)
	}
}

func TestSendNilHTTPClientUsesBrokerDefault(t *testing.T) {
	type receivedRequest struct {
		method    string
		path      string
		clientID  string
		sessionID string
	}
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receivedRequest{
			method:    r.Method,
			path:      r.URL.Path,
			clientID:  r.Header.Get("X-OpenRung-Client-ID"),
			sessionID: r.Header.Get("X-OpenRung-Session-ID"),
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	events := []Event{{EventID: "e1", ClientID: "client-1", SessionID: "session-1", Event: "x"}}
	if err := (HTTPClient{BaseURL: server.URL}).Send(t.Context(), events); err != nil {
		t.Fatalf("send with nil HTTP client: %v", err)
	}

	got := <-received
	if got.method != http.MethodPost || got.path != "/api/v1/telemetry/events" {
		t.Fatalf("request = %s %s, want POST /api/v1/telemetry/events", got.method, got.path)
	}
	if got.clientID != "client-1" || got.sessionID != "session-1" {
		t.Fatalf("identity headers = client %q, session %q", got.clientID, got.sessionID)
	}
}

func TestSendRefusesRedirectWithoutReplayingIdentity(t *testing.T) {
	var originRequests atomic.Int64
	var redirectedRequests atomic.Int64
	var originSawIdentity atomic.Bool
	var redirectLeakedIdentity atomic.Bool

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedRequests.Add(1)
		if r.Header.Get("X-OpenRung-Client-ID") != "" || r.Header.Get("X-OpenRung-Session-ID") != "" {
			redirectLeakedIdentity.Store(true)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer redirectTarget.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		if r.Header.Get("X-OpenRung-Client-ID") == "client-1" &&
			r.Header.Get("X-OpenRung-Session-ID") == "session-1" {
			originSawIdentity.Store(true)
		}
		http.Redirect(w, r, redirectTarget.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	events := []Event{{EventID: "e1", ClientID: "client-1", SessionID: "session-1", Event: "x"}}
	err := (HTTPClient{BaseURL: origin.URL}).Send(t.Context(), events)
	if err == nil {
		t.Fatal("redirected telemetry send succeeded, want redirect refusal")
	}
	if !originSawIdentity.Load() {
		t.Fatal("origin did not receive the expected identity headers")
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("origin requests = %d, want 1", got)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
	if redirectLeakedIdentity.Load() {
		t.Fatal("redirect replay leaked persistent identity headers")
	}
}

func TestSendEmptyIsNoOp(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("send must not make a request for an empty batch")
		return nil, nil
	})}
	if err := (HTTPClient{BaseURL: "https://broker.example.com", HTTP: httpClient}).Send(context.Background(), nil); err != nil {
		t.Fatalf("send empty: %v", err)
	}
}

func TestSendNon2xxSurfacesError(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(r, http.StatusBadRequest, `{"error":"schema_version must be 1"}`), nil
	})}
	err := HTTPClient{BaseURL: "https://broker.example.com", HTTP: httpClient}.
		Send(context.Background(), []Event{{EventID: "e1", ClientID: "c", SessionID: "s"}})
	if err == nil || !strings.Contains(err.Error(), "schema_version must be 1") {
		t.Fatalf("expected broker error, got %v", err)
	}
}
