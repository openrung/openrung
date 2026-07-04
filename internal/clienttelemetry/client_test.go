package clienttelemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
