package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestWSSTicketURL(t *testing.T) {
	got, err := WSSTicketURL("https://broker.example.com/base/?unrelated=secret#fragment")
	if err != nil {
		t.Fatalf("build WSS ticket URL: %v", err)
	}
	if want := "https://broker.example.com/base/api/v1/wss/tickets"; got != want {
		t.Fatalf("WSSTicketURL = %q, want %q", got, want)
	}

	if _, err := WSSTicketURL("http://broker.example.com/"); err == nil {
		t.Fatal("WSSTicketURL accepted cleartext non-loopback broker")
	}
	if got, err := WSSTicketURL("http://localhost:8080/"); err != nil || got != "http://localhost:8080/api/v1/wss/tickets" {
		t.Fatalf("loopback WSSTicketURL = %q, %v", got, err)
	}
}

func TestBrokerClientRequestWSSSessionTicket(t *testing.T) {
	expires := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	wantResponse := relay.WSSSessionTicketResponse{
		Ticket:    "opaque-ticket",
		ExpiresAt: expires,
		URL:       "wss://bridge.example.com/api/v1/bridge",
	}
	var requests int
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/base/api/v1/wss/tickets" {
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if got := r.Header.Get("Pragma"); got != "no-cache" {
			t.Fatalf("Pragma = %q", got)
		}
		if got := r.Header.Get("X-OpenRung-Client-ID"); got != "client-1" {
			t.Fatalf("client identity header = %q", got)
		}
		if got := r.Header.Get("X-OpenRung-Session-ID"); got != "session-1" {
			t.Fatalf("session identity header = %q", got)
		}
		if got := r.Header.Get("X-OpenRung-App-Version"); got == "" {
			t.Fatal("missing application version header")
		}

		var gotRequest relay.WSSSessionTicketRequest
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if gotRequest.RelayID != "relay-1" || gotRequest.FrontID != "gateway-1" {
			t.Fatalf("request body = %+v", gotRequest)
		}

		body, err := json.Marshal(wantResponse)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    r,
		}, nil
	})}

	client := BrokerClient{BaseURL: "https://broker.example.com/base/", HTTPClient: httpClient}
	got, err := client.RequestWSSSessionTicket(t.Context(), relay.WSSSessionTicketRequest{
		RelayID: "relay-1",
		FrontID: "gateway-1",
	}, "client-1", "session-1")
	if err != nil {
		t.Fatalf("request WSS ticket: %v", err)
	}
	if got.Ticket != wantResponse.Ticket || !got.ExpiresAt.Equal(expires) || got.URL != wantResponse.URL {
		t.Fatalf("ticket response = %+v, want %+v", got, wantResponse)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestBrokerClientRequestWSSSessionTicketOmitsIncompleteIdentity(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-OpenRung-Client-ID") != "" || r.Header.Get("X-OpenRung-Session-ID") != "" {
			t.Fatalf("incomplete identity was sent: %+v", r.Header)
		}
		if r.Header.Get("X-OpenRung-App-Version") == "" {
			t.Fatal("application version must be sent without identity headers")
		}
		body, _ := json.Marshal(relay.WSSSessionTicketResponse{
			Ticket: "ticket", ExpiresAt: expires, URL: "wss://bridge.example.com/bridge",
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    r,
		}, nil
	})}

	_, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).RequestWSSSessionTicket(
		t.Context(), relay.WSSSessionTicketRequest{RelayID: "relay-1", FrontID: "gateway-1"}, "client-only", "",
	)
	if err != nil {
		t.Fatalf("request WSS ticket: %v", err)
	}
}

func TestBrokerClientRequestWSSSessionTicketValidatesRequestBeforeHTTP(t *testing.T) {
	requests := 0
	client := BrokerClient{
		BaseURL: "https://broker.example.com",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			return nil, errors.New("unexpected request")
		})},
	}

	for _, request := range []relay.WSSSessionTicketRequest{
		{FrontID: "gateway-1"},
		{RelayID: "relay-1"},
	} {
		if _, err := client.RequestWSSSessionTicket(t.Context(), request, "", ""); err == nil {
			t.Fatalf("RequestWSSSessionTicket(%+v) succeeded", request)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid requests reached HTTP transport %d times", requests)
	}
}

func TestBrokerClientRequestWSSTicketStatusErrorIgnoresOriginBody(t *testing.T) {
	secret := "origin-controlled-ticket-secret\nforged log line"
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Retry-After", "17")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(secret)),
			Request:    r,
		}, nil
	})}

	_, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).RequestWSSSessionTicket(
		t.Context(), relay.WSSSessionTicketRequest{RelayID: "relay-1", FrontID: "gateway-1"}, "", "",
	)
	var statusErr *WSSTicketStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T %v, want WSSTicketStatusError", err, err)
	}
	if statusErr.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d", statusErr.HTTPStatus())
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "forged log line") {
		t.Fatalf("status error exposed origin body: %q", err)
	}
	if statusErr.RetryAfter != 17*time.Second {
		t.Fatalf("RetryAfter = %s, want 17s", statusErr.RetryAfter)
	}
}

func TestParseWSSTicketRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: " 12 ", want: 12 * time.Second},
		{name: "date", value: now.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second},
		{name: "past", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "negative", value: "-1"},
		{name: "overflow", value: "9223372036854775807"},
		{name: "invalid", value: "later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWSSTicketRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("parseWSSTicketRetryAfter(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

func TestBrokerClientRequestWSSSessionTicketRejectsOversizedResponse(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxWSSTicketResponseBytes+1))),
			Request:    r,
		}, nil
	})}

	_, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).RequestWSSSessionTicket(
		context.Background(), relay.WSSSessionTicketRequest{RelayID: "relay-1", FrontID: "gateway-1"}, "", "",
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestBrokerClientRequestWSSSessionTicketRejectsIncompleteResponse(t *testing.T) {
	for _, body := range []string{
		`{"expires_at":"2026-07-21T13:00:00Z","url":"wss://bridge.example.com/bridge"}`,
		`{"ticket":"ticket","url":"wss://bridge.example.com/bridge"}`,
		`{"ticket":"ticket","expires_at":"2026-07-21T13:00:00Z"}`,
	} {
		t.Run(body, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			})}
			_, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).RequestWSSSessionTicket(
				t.Context(), relay.WSSSessionTicketRequest{RelayID: "relay-1", FrontID: "gateway-1"}, "", "",
			)
			if err == nil {
				t.Fatal("incomplete response succeeded")
			}
		})
	}
}

func TestBrokerClientRequestWSSSessionTicketRejectsRedirectWithoutLeakingIdentity(t *testing.T) {
	requests := 0
	var leakedIdentity bool
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 || r.URL.Host == "attacker.example" {
			leakedIdentity = r.Header.Get("X-OpenRung-Client-ID") != "" || r.Header.Get("X-OpenRung-Session-ID") != ""
		}
		header := make(http.Header)
		header.Set("Location", "http://attacker.example/collect")
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Status:     "307 Temporary Redirect",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    r,
		}, nil
	})}

	_, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).RequestWSSSessionTicket(
		t.Context(), relay.WSSSessionTicketRequest{RelayID: "relay-1", FrontID: "gateway-1"}, "client-1", "session-1",
	)
	var statusErr *WSSTicketStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect error = %T %v", err, err)
	}
	if requests != 1 {
		t.Fatalf("redirect made %d requests, want exactly one", requests)
	}
	if leakedIdentity {
		t.Fatal("redirect leaked identity headers to a second origin")
	}
}
