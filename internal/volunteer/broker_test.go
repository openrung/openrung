package volunteer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"openrung/internal/relay"
)

func TestBrokerClientSecureTransportAllowsLoopbackHTTP(t *testing.T) {
	httpClient := &http.Client{Transport: brokerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer foundation-secret" {
			t.Errorf("Authorization = %q, want foundation bearer", got)
		}
		switch r.URL.Path {
		case "/api/v1/volunteers/register":
			body, err := json.Marshal(relay.Descriptor{ID: "relay_foundation"})
			if err != nil {
				return nil, err
			}
			return brokerJSONResponse(r, http.StatusCreated, string(body)), nil
		case "/api/v1/volunteers/relay_foundation/heartbeat":
			return brokerJSONResponse(r, http.StatusOK, `{}`), nil
		default:
			return brokerJSONResponse(r, http.StatusNotFound, `{"error":"not found"}`), nil
		}
	})}

	client := BrokerClient{
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
		do   func(BrokerClient) error
	}{
		{
			name: "register",
			do: func(client BrokerClient) error {
				_, err := client.Register(context.Background(), relay.RegisterRequest{})
				return err
			},
		},
		{
			name: "heartbeat",
			do: func(client BrokerClient) error {
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
			client := BrokerClient{
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
	client := BrokerClient{
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

func brokerJSONResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
