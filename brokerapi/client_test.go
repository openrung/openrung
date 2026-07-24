package brokerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestEnforceSecureBrokerURL(t *testing.T) {
	accepted := []string{
		"https://broker.openrung.org/",
		"https://192.0.2.1/custom",
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	}
	for _, raw := range accepted {
		if _, err := EnforceSecureBrokerURL(raw); err != nil {
			t.Errorf("%q rejected: %v", raw, err)
		}
	}
	rejected := []string{
		"",
		"broker.openrung.org",
		"http://broker.openrung.org",
		"http://localhost.example",
		"http://127.0.0.2.example",
		"ftp://localhost",
		"https://user:password@broker.openrung.org",
	}
	for _, raw := range rejected {
		if _, err := EnforceSecureBrokerURL(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestEndpointBuildersPreserveBasePathAndDiscardUnrelatedURLState(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) (string, error)
		want string
	}{
		{"relay", func(base string) (string, error) { return RelayListURL(base, 0) }, "/prefix/api/v1/relays?limit=5"},
		{"telemetry", TelemetryURL, "/prefix/api/v1/telemetry/events"},
		{"ticket", WSSTicketURL, "/prefix/api/v1/wss/tickets"},
		{"health", HealthURL, "/prefix/healthz"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.fn("https://broker.example/prefix?secret=discard#fragment")
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.RequestURI() != test.want {
				t.Fatalf("request URI = %q, want %q", parsed.RequestURI(), test.want)
			}
			if parsed.Fragment != "" {
				t.Fatal("fragment retained")
			}
		})
	}
}

func TestListRelaysAppliesSharedHeadersAndAtomicIdentity(t *testing.T) {
	var requestSeen *http.Request
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestSeen = request.Clone(request.Context())
		return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
	})
	api := NewClient(&http.Client{Transport: transport}, Options{
		AppVersion: "1.2.3",
		Platform:   PlatformDesktop,
	})
	list, err := api.ListRelays(context.Background(), "http://127.0.0.1:8080/base", ListOptions{
		Limit: 7,
		Identity: Identity{
			ClientID:  "client-1",
			SessionID: "session-1",
		},
	})
	if err != nil {
		t.Fatalf("ListRelays: %v", err)
	}
	if string(list.JSON()) != `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}` || list.SignatureVerified {
		t.Fatalf("list = %+v", list)
	}
	if requestSeen.URL.RequestURI() != "/base/api/v1/relays?limit=7" {
		t.Fatalf("request URI = %q", requestSeen.URL.RequestURI())
	}
	assertHeader(t, requestSeen.Header, "Cache-Control", "no-cache, no-store")
	assertHeader(t, requestSeen.Header, "Pragma", "no-cache")
	assertHeader(t, requestSeen.Header, "X-OpenRung-App-Version", "1.2.3")
	assertHeader(t, requestSeen.Header, "X-OpenRung-Desktop", platformHeaderValue(PlatformDesktop, ""))
	assertHeader(t, requestSeen.Header, "X-OpenRung-Client-ID", "client-1")
	assertHeader(t, requestSeen.Header, "X-OpenRung-Session-ID", "session-1")

	requestSeen = nil
	_, err = api.ListRelays(context.Background(), "http://127.0.0.1:8080", ListOptions{
		Identity: Identity{ClientID: "client-only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestSeen.Header.Get("X-OpenRung-Client-ID") != "" ||
		requestSeen.Header.Get("X-OpenRung-Session-ID") != "" {
		t.Fatal("partial identity was emitted")
	}
}

func TestListRelaysNeverFollowsRedirectOrLeaksIdentity(t *testing.T) {
	var calls atomic.Int64
	var secondRequest *http.Request
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) > 1 {
			secondRequest = request
			return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
		}
		resp := response(request, http.StatusFound, "")
		resp.Header.Set("Location", "http://attacker.example/collect")
		return resp, nil
	})
	api := NewClient(&http.Client{Transport: transport}, Options{})
	_, err := api.ListRelays(context.Background(), "http://127.0.0.1:8080", ListOptions{
		Identity: Identity{ClientID: "client", SessionID: "session"},
	})
	var statusErr *BrokerStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusFound {
		t.Fatalf("redirect error = %T %v", err, err)
	}
	if calls.Load() != 1 || secondRequest != nil {
		t.Fatal("relay redirect was followed")
	}
}

func TestListRelaysBoundsResponseAndStatusBody(t *testing.T) {
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, strings.Repeat("x", maxRelayListBytes+1)), nil
	})}, Options{})
	if _, err := api.ListRelays(context.Background(), "http://127.0.0.1:8080", ListOptions{}); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v", err)
	}

	api = NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusBadRequest, `{"error":"bad request"}`), nil
	})}, Options{})
	_, err := api.ListRelays(context.Background(), "http://127.0.0.1:8080", ListOptions{})
	var statusErr *BrokerStatusError
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != 400 || statusErr.Message != "bad request" {
		t.Fatalf("status error = %#v (%v)", statusErr, err)
	}
}

func TestFirstReachableStaggersAndCancelsLoser(t *testing.T) {
	var fallbackStarted atomic.Bool
	primaryCanceled := make(chan struct{})
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Port() {
		case "8001":
			<-request.Context().Done()
			close(primaryCanceled)
			return nil, request.Context().Err()
		case "8002":
			fallbackStarted.Store(true)
			return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
		default:
			return nil, errors.New("unexpected candidate")
		}
	})
	api := NewClient(&http.Client{Transport: transport}, Options{})
	fetch, err := api.FirstReachable(context.Background(), Candidates{
		URLs: []string{"http://127.0.0.1:8001", "http://127.0.0.1:8002"},
	}, ListOptions{Stagger: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if fetch.BrokerURL != "http://127.0.0.1:8002" || !fallbackStarted.Load() {
		t.Fatalf("fetch = %+v", fetch)
	}
	select {
	case <-primaryCanceled:
	case <-time.After(time.Second):
		t.Fatal("winning candidate did not cancel loser")
	}
}

func TestFirstReachableParentCancellationReturnsWithoutDrainingTransport(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		// Deliberately model a custom transport that is slow to observe the
		// canceled request context. FirstReachable must not wait for it.
		<-release
		return nil, errors.New("transport eventually stopped")
	})}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := api.FirstReachable(ctx, Candidates{
			URLs: []string{"http://127.0.0.1:8001"},
		}, ListOptions{})
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled race returned %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("canceled race waited for its transport to finish")
	}
}

func TestFirstReachableHonorsStrictOverride(t *testing.T) {
	var mu sync.Mutex
	var order []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		order = append(order, request.URL.Port())
		mu.Unlock()
		if request.URL.Port() == "8001" {
			time.Sleep(30 * time.Millisecond)
			return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
		}
		return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
	})
	api := NewClient(&http.Client{Transport: transport}, Options{})
	fetch, err := api.FirstReachable(context.Background(), Candidates{
		URLs:          []string{"http://127.0.0.1:8001", "http://127.0.0.1:8002"},
		OverrideFirst: true,
	}, ListOptions{Stagger: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if fetch.BrokerURL != "http://127.0.0.1:8001" {
		t.Fatalf("override lost: %+v", fetch)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "8001" {
		t.Fatalf("request order = %v", order)
	}
}

func TestFirstReachableRejectsMalformedPrimaryBeforeItCanWin(t *testing.T) {
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Port() == "8001" {
			return response(request, http.StatusOK, `{"relays":[{"id":"bad","public_port":"not-an-integer"}]}`), nil
		}
		return response(request, http.StatusOK, `{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`), nil
	})
	api := NewClient(&http.Client{Transport: transport}, Options{})
	fetch, err := api.FirstReachable(context.Background(), Candidates{
		URLs: []string{"http://127.0.0.1:8001", "http://127.0.0.1:8002"},
	}, ListOptions{Stagger: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if fetch.BrokerURL != "http://127.0.0.1:8002" || calls.Load() != 2 {
		t.Fatalf("malformed primary won or blocked fallback: fetch=%+v calls=%d", fetch, calls.Load())
	}
}

func TestSendTelemetryUsesSharedPolicy(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(request.Body)
		gotHeader = request.Header.Clone()
		return response(request, http.StatusNoContent, ""), nil
	})}, Options{AppVersion: "2.0.0", Platform: PlatformReactNative, PlatformVersion: "ios"})
	if err := api.SendTelemetry(context.Background(), "http://127.0.0.1:8080", []TelemetryEvent{{
		ClientID: "c", SessionID: "s",
	}}); err != nil {
		t.Fatal(err)
	}
	var batch telemetryBatch
	if err := json.Unmarshal(gotBody, &batch); err != nil || len(batch.Events) != 1 {
		t.Fatalf("body = %s (%v)", gotBody, err)
	}
	assertHeader(t, gotHeader, "Content-Type", "application/json")
	assertHeader(t, gotHeader, "Cache-Control", "no-cache, no-store")
	assertHeader(t, gotHeader, "X-OpenRung-RN", "ios")
	assertHeader(t, gotHeader, "X-OpenRung-Client-ID", "c")
}

func TestSendTelemetryValidatesBatchIdentityAndLimits(t *testing.T) {
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusNoContent, ""), nil
	})}, Options{})
	events := []TelemetryEvent{
		{ClientID: "a", SessionID: "s"},
		{ClientID: "b", SessionID: "s"},
	}
	if err := api.SendTelemetry(context.Background(), "http://127.0.0.1:8080", events); err == nil {
		t.Fatal("mixed telemetry identities accepted")
	}
	if err := api.SendTelemetryBatchJSON(context.Background(), "http://127.0.0.1:8080", []byte(`{"events":[]} trailing`)); err == nil {
		t.Fatal("trailing telemetry JSON accepted")
	}
}

func TestSendTelemetryBatchJSONScrubsLegacyDestinationFields(t *testing.T) {
	var sent []byte
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(request.Body)
		return response(request, http.StatusNoContent, ""), nil
	})}, Options{})
	legacy := []byte(`{"events":[{"schema_version":1,"event_id":"e","event":"application_connection","occurred_at":"2026-07-24T00:00:00Z","client_id":"c","session_id":"s","application_package":"org.example","destination_ip":"203.0.113.1","destination_port":443,"protocol":"tcp"}]}`)
	if err := api.SendTelemetryBatchJSON(context.Background(), "http://127.0.0.1:8080", legacy); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("destination_ip"), []byte("destination_port"), []byte(`"protocol"`), []byte("203.0.113.1")} {
		if bytes.Contains(sent, forbidden) {
			t.Fatalf("telemetry upload retained privacy-sensitive field %q: %s", forbidden, sent)
		}
	}
	if !bytes.Contains(sent, []byte("application_package")) {
		t.Fatalf("application identity was lost while scrubbing: %s", sent)
	}
}

func TestRequestWSSTicketValidatesAndExcludesIdentityFromBody(t *testing.T) {
	var body []byte
	var headers http.Header
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(request.Body)
		headers = request.Header.Clone()
		return response(request, http.StatusOK, `{"ticket":"opaque","expires_at":"2099-01-01T00:00:00Z","url":"wss://relay.example/bridge"}`), nil
	})}, Options{Platform: PlatformAndroid, PlatformVersion: "35"})
	ticket, err := api.RequestWSSTicket(context.Background(), "http://127.0.0.1:8080", WSSTicketRequest{
		RelayID:  "relay-1",
		FrontID:  "front-1",
		Identity: Identity{ClientID: "client", SessionID: "session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Ticket != "opaque" || bytes.Contains(body, []byte("client")) {
		t.Fatalf("ticket/body = %+v %s", ticket, body)
	}
	var wire map[string]string
	if err := json.Unmarshal(body, &wire); err != nil || len(wire) != 2 {
		t.Fatalf("wire body = %s (%v)", body, err)
	}
	assertHeader(t, headers, "X-OpenRung-Android-API", "35")
	assertHeader(t, headers, "X-OpenRung-Client-ID", "client")

	if _, err := api.RequestWSSTicket(context.Background(), "http://127.0.0.1:8080", WSSTicketRequest{}); err == nil {
		t.Fatal("empty ticket request accepted")
	}
}

func TestRequestWSSTicketRejectsUnsafeResponsesAndRedactsBearer(t *testing.T) {
	tests := []struct {
		name   string
		ticket WSSTicketResponse
	}{
		{
			name: "oversized",
			ticket: WSSTicketResponse{
				Ticket: strings.Repeat("t", maxWSSTicketBytes+1), ExpiresAt: time.Now().Add(time.Hour), URL: "wss://relay.example/bridge",
			},
		},
		{
			name: "header injection",
			ticket: WSSTicketResponse{
				Ticket: "opaque\r\ninjected", ExpiresAt: time.Now().Add(time.Hour), URL: "wss://relay.example/bridge",
			},
		},
		{
			name: "expired",
			ticket: WSSTicketResponse{
				Ticket: "opaque", ExpiresAt: time.Now().Add(-time.Minute), URL: "wss://relay.example/bridge",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.ticket)
			if err != nil {
				t.Fatal(err)
			}
			api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return response(request, http.StatusOK, string(body)), nil
			})}, Options{})
			if _, err := api.RequestWSSTicket(context.Background(), "http://127.0.0.1:8080", WSSTicketRequest{
				RelayID: "r", FrontID: "f",
			}); err == nil {
				t.Fatalf("unsafe WSS ticket response accepted: %s", body)
			}
		})
	}

	ticket := WSSTicketResponse{
		Ticket: "secret-bearer", ExpiresAt: time.Now().Add(time.Hour), URL: "wss://relay.example/bridge",
	}
	for _, rendered := range []string{fmt.Sprint(ticket), fmt.Sprintf("%+v", ticket), fmt.Sprintf("%#v", ticket)} {
		if strings.Contains(rendered, ticket.Ticket) || !strings.Contains(rendered, "<redacted>") {
			t.Fatalf("formatted ticket was not redacted: %q", rendered)
		}
	}
}

func TestRequestWSSTicketStatusDoesNotExposeBody(t *testing.T) {
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		resp := response(request, http.StatusTooManyRequests, `{"error":"secret-origin-text"}`)
		resp.Header.Set("Retry-After", "10")
		return resp, nil
	})}, Options{})
	_, err := api.RequestWSSTicket(context.Background(), "http://127.0.0.1:8080", WSSTicketRequest{
		RelayID: "r", FrontID: "f",
	})
	var statusErr *WSSTicketStatusError
	if !errors.As(err, &statusErr) || statusErr.RetryAfter != 10*time.Second {
		t.Fatalf("ticket status error = %#v (%v)", statusErr, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("WSS ticket status exposed response body")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{" 12 ", 12 * time.Second},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"-1", 0},
		{"86401", 0},
		{now.Add(25 * time.Hour).Format(http.TimeFormat), 0},
		{"later", 0},
	}
	for _, test := range tests {
		if got := parseRetryAfter(test.value, now); got != test.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", test.value, got, test.want)
		}
	}
}

func TestProbeHealthAndStreamingSpeedTest(t *testing.T) {
	api := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/healthz":
			return response(request, http.StatusNotFound, ""), nil
		case "/api/v1/speed-test":
			if request.Header.Get("Accept-Encoding") != "identity" {
				t.Error("speed test did not disable content encoding")
			}
			if request.URL.Query().Get("bytes") != "8" || request.URL.Query().Get("cacheBust") == "" {
				t.Errorf("speed-test query = %q", request.URL.RawQuery)
			}
			return response(request, http.StatusOK, "12345678"), nil
		default:
			return response(request, http.StatusNotFound, ""), nil
		}
	})}, Options{Platform: PlatformIOS, PlatformVersion: "18.5"})
	if err := api.ProbeHealth(context.Background(), "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("404 should prove reachability: %v", err)
	}
	result, err := api.DownloadSpeedTest(context.Background(), "http://127.0.0.1:8080", 8)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != 8 || result.TotalDuration <= 0 || result.MegabitsPerSecond <= 0 {
		t.Fatalf("speed result = %+v", result)
	}
}

func TestBrokerCandidatesReturnsFreshDefaults(t *testing.T) {
	first := BrokerCandidates("")
	first.URLs[0] = "mutated"
	second := BrokerCandidates(DefaultBrokerURL)
	if second.OverrideFirst || second.URLs[0] != DefaultBrokerURL {
		t.Fatalf("defaults mutated or reordered: %+v", second)
	}
	custom := BrokerCandidates(" https://custom.example/ ")
	if !custom.OverrideFirst || custom.URLs[0] != "https://custom.example/" {
		t.Fatalf("custom candidates = %+v", custom)
	}
}

func assertHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	if got := header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
