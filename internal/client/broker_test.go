package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestRelayListURL(t *testing.T) {
	got, err := RelayListURL("https://broker.example.com/base/", 7)
	if err != nil {
		t.Fatalf("build relay list URL: %v", err)
	}
	want := "https://broker.example.com/base/api/v1/relays?limit=7"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBrokerClientListRelays(t *testing.T) {
	// Non-loopback responses must be signed (signing.go), so the stub signs
	// with the shared test-vector key pinned for the test's duration.
	vectors := loadVectors(t)
	restore := PinRelayListKeysForTest(vectors.SpecVector.PubKeyHex)
	defer restore()

	now := time.Now().UTC()
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/relays" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "3" {
			t.Fatalf("unexpected limit %q", r.URL.Query().Get("limit"))
		}
		body, header := signedListBody(t, vectorSigner(t, vectors), vectors.SpecVector.KeyID, relay.ListResponse{
			Count:      1,
			ServerTime: now,
			NotAfter:   now.Add(30 * time.Minute),
			KeyID:      vectors.SpecVector.KeyID,
			Channel:    relay.ChannelAPI,
			Limit:      3,
			Relays:     []relay.Descriptor{validRelay(now)},
		})
		h := make(http.Header)
		h.Set(RelaySignatureHeader, header)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Header:     h,
			Request:    r,
		}, nil
	})}

	resp, err := BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}.ListRelays(context.Background(), 3, "", "")
	if err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if resp.Count != 1 || len(resp.Relays) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestBrokerClientListRelaysNon2xx(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := json.Marshal(relay.ErrorResponse{Error: "broker unavailable"})
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	_, err := BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}.ListRelays(context.Background(), 5, "", "")
	if err == nil {
		t.Fatal("expected broker error")
	}
}

func TestBrokerClientListRelaysSetsIdentityHeaders(t *testing.T) {
	vectors := loadVectors(t)
	restore := PinRelayListKeysForTest(vectors.SpecVector.PubKeyHex)
	defer restore()

	now := time.Now().UTC()
	var gotClientID, gotSessionID, gotAppVersion string
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotClientID = r.Header.Get("X-OpenRung-Client-ID")
		gotSessionID = r.Header.Get("X-OpenRung-Session-ID")
		gotAppVersion = r.Header.Get("X-OpenRung-App-Version")
		body, header := signedListBody(t, vectorSigner(t, vectors), vectors.SpecVector.KeyID, relay.ListResponse{
			Count:      0,
			ServerTime: now,
			NotAfter:   now.Add(30 * time.Minute),
			KeyID:      vectors.SpecVector.KeyID,
			Channel:    relay.ChannelAPI,
			Limit:      5,
		})
		h := make(http.Header)
		h.Set(RelaySignatureHeader, header)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Header:     h,
			Request:    r,
		}, nil
	})}

	client := BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}
	if _, err := client.ListRelays(context.Background(), 5, "client-1", "session-1"); err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if gotClientID != "client-1" || gotSessionID != "session-1" {
		t.Fatalf("expected identity headers, got client=%q session=%q", gotClientID, gotSessionID)
	}
	if gotAppVersion == "" {
		t.Fatal("expected app version header")
	}

	if _, err := client.ListRelays(context.Background(), 5, "", ""); err != nil {
		t.Fatalf("list relays: %v", err)
	}
	if gotClientID != "" || gotSessionID != "" {
		t.Fatalf("expected no identity headers when empty, got client=%q session=%q", gotClientID, gotSessionID)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
