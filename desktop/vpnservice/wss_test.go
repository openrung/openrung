package vpnservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/internal/client"
	"openrung/internal/relay"
)

const (
	testWSSFrontAURL = "wss://a.cdn.example/api/v1/wss-bridge"
	testWSSFrontBURL = "wss://b.cdn.example/api/v1/wss-bridge"
)

type fakeWSSBridge struct {
	host string
	port int

	fatal   chan error
	started chan struct{}
	exited  chan struct{}

	startOnce  sync.Once
	exitOnce   sync.Once
	closeCalls atomic.Int32
}

func newFakeWSSBridge() *fakeWSSBridge {
	return &fakeWSSBridge{
		host: "127.0.0.1", port: 43123,
		fatal: make(chan error, 1), started: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (b *fakeWSSBridge) Endpoint() (string, int) { return b.host, b.port }

func (b *fakeWSSBridge) Serve(ctx context.Context) error {
	b.startOnce.Do(func() { close(b.started) })
	defer b.exitOnce.Do(func() { close(b.exited) })
	select {
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (b *fakeWSSBridge) Close() error {
	b.closeCalls.Add(1)
	return nil
}

func testWSSFront(id, rawURL string) relay.WSSFrontDescriptor {
	return relay.WSSFrontDescriptor{ID: id, URL: rawURL, ProtocolVersion: relay.WSSProtocolVersion}
}

func relayWithWSS(id, countryCode, city, country, host string, fronts ...relay.WSSFrontDescriptor) relay.Descriptor {
	candidate := relayAt(id, countryCode, city, country, host)
	candidate.NodeClass = relay.NodeClassFoundation
	candidate.Transport = relay.TransportDirect
	if len(fronts) == 0 {
		fronts = []relay.WSSFrontDescriptor{testWSSFront("front-a", testWSSFrontAURL)}
	}
	candidate.WSSFronts = fronts
	return candidate
}

func successfulWSSTicket(front relay.WSSFrontDescriptor, value string) relay.WSSSessionTicketResponse {
	return relay.WSSSessionTicketResponse{Ticket: value, ExpiresAt: time.Now().Add(time.Minute), URL: front.URL}
}

func waitWSSSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestSupportedWSSFrontsRequiresCanonicalEligibleRelay(t *testing.T) {
	frontA := testWSSFront("front-a", testWSSFrontAURL)
	frontB := testWSSFront("front-b", testWSSFrontBURL)
	eligible := relayWithWSS("relay-a", "IR", "Tehran", "Iran", "192.0.2.10", frontA, frontB)
	if got := supportedWSSFronts(eligible); !reflect.DeepEqual(got, []relay.WSSFrontDescriptor{frontA, frontB}) {
		t.Fatalf("supported fronts = %+v", got)
	}

	tests := map[string]func(*relay.Descriptor){
		"volunteer": func(candidate *relay.Descriptor) { candidate.NodeClass = relay.NodeClassVolunteer },
		"tunnel":    func(candidate *relay.Descriptor) { candidate.Transport = relay.TransportTunnel },
		"exit mode": func(candidate *relay.Descriptor) { candidate.ExitMode = relay.ExitModeDedicated },
		"port":      func(candidate *relay.Descriptor) { candidate.PublicPort = 4443 },
		"reordered": func(candidate *relay.Descriptor) {
			candidate.WSSFronts[0], candidate.WSSFronts[1] = candidate.WSSFronts[1], candidate.WSSFronts[0]
		},
		"bad URL": func(candidate *relay.Descriptor) {
			candidate.WSSFronts[0].URL = "wss://shared.example/another-path"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := eligible
			candidate.WSSFronts = append([]relay.WSSFrontDescriptor(nil), eligible.WSSFronts...)
			mutate(&candidate)
			if got := supportedWSSFronts(candidate); len(got) != 0 {
				t.Fatalf("unsafe fronts accepted: %+v", got)
			}
		})
	}
}

func TestWSSTicketBrokerFailoverAndBoundedRetryAfter(t *testing.T) {
	servingFront := config.DefaultBrokerURLs[1]
	fronts := wssTicketBrokerFronts(servingFront)
	if len(fronts) != len(config.DefaultBrokerURLs) || fronts[0] != servingFront || fronts[1] != config.DefaultBrokerURLs[0] {
		t.Fatalf("ticket broker fronts = %v", fronts)
	}

	s := New()
	conn := &connection{brokerURL: servingFront}
	var calls []string
	s.requestWSSTicket = func(_ context.Context, brokerURL string, request relay.WSSSessionTicketRequest, _, _ string) (relay.WSSSessionTicketResponse, error) {
		calls = append(calls, brokerURL)
		if request.RelayID != "relay-a" || request.FrontID != "front-a" {
			t.Fatalf("ticket request = %+v", request)
		}
		if len(calls) <= len(fronts) {
			return relay.WSSSessionTicketResponse{}, &client.WSSTicketStatusError{
				StatusCode: 429, RetryAfter: time.Minute,
			}
		}
		return successfulWSSTicket(testWSSFront("front-a", testWSSFrontAURL), "new-ticket"), nil
	}
	var waits []time.Duration
	s.waitWSSRetry = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	response, err := s.requestWSSSessionTicket(t.Context(), conn, relay.WSSSessionTicketRequest{RelayID: "relay-a", FrontID: "front-a"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Ticket != "new-ticket" || len(calls) != len(fronts)+1 {
		t.Fatalf("response=%+v calls=%v", response, calls)
	}
	if len(waits) != 1 || waits[0] != wssTicketMaxRetry {
		t.Fatalf("bounded Retry-After waits = %v", waits)
	}
	if !conn.wssTicketRetryUsed {
		t.Fatal("one-per-ladder retry was not consumed")
	}
}

func TestWSSDirectSuccessNeverRequestsTicket(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	var tickets, dials atomic.Int32
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		tickets.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "unused"), nil
	}
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		dials.Add(1)
		return newFakeWSSBridge(), nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	if tickets.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("direct success used WSS: tickets=%d dials=%d", tickets.Load(), dials.Load())
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestWSSLocalSetupFailuresDoNotRequestTicketsOrDamageHealth(t *testing.T) {
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	tests := map[string]func(*Service, chan struct{}) relay.Descriptor{
		"config": func(_ *Service, _ chan struct{}) relay.Descriptor {
			invalid := fixture
			invalid.ClientID = ""
			return invalid
		},
		"temp file": func(service *Service, _ chan struct{}) relay.Descriptor {
			service.writeConfig = func([]byte) (string, error) { return "", errors.New("temporary config unavailable") }
			return fixture
		},
		"start or bind": func(service *Service, _ chan struct{}) relay.Descriptor {
			service.runTunnel = func(context.Context, string) error { return errors.New("sing-box could not bind local inbound") }
			return fixture
		},
		"ready timeout": func(service *Service, _ chan struct{}) relay.Descriptor {
			service.tunnelReady = func(context.Context, int) error { return errors.New("local inbound not ready") }
			service.tunnelReadyLimit = 30 * time.Millisecond
			return fixture
		},
		"process exit during probe": func(service *Service, probeStarted chan struct{}) relay.Descriptor {
			exit := make(chan struct{})
			service.runTunnel = func(ctx context.Context, _ string) error {
				select {
				case <-exit:
					return errors.New("sing-box exited during probe")
				case <-ctx.Done():
					return nil
				}
			}
			service.probeTunnel = func(ctx context.Context, _ int) (int64, error) {
				close(probeStarted)
				close(exit)
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return fixture
		},
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			sink := newTelemetrySink(t)
			probeStarted := make(chan struct{})
			var candidate relay.Descriptor
			s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{candidate} })
			candidate = configure(s, probeStarted)
			var tickets atomic.Int32
			s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
				tickets.Add(1)
				return successfulWSSTicket(fixture.WSSFronts[0], "unused"), nil
			}
			if err := s.Connect(sink.srv.URL, "", ""); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, s, StatusFailed)
			waitIdle(t, s)
			if tickets.Load() != 0 {
				t.Fatalf("local failure requested %d ticket(s)", tickets.Load())
			}
			if attempts := sink.named("relay_attempt_failed"); len(attempts) != 0 {
				t.Fatalf("local failure damaged relay health: %+v", attempts)
			}
		})
	}
}

func TestWSSConfigBuildFailureDoesNotUnlockFallback(t *testing.T) {
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	fixture.ClientID = "" // bypass discovery to exercise the config boundary itself
	s, _ := newLadderService(t, func() []relay.Descriptor { return nil })
	var tickets atomic.Int32
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		tickets.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "unused"), nil
	}

	_, err := s.attemptCandidate(t.Context(), &connection{brokerURL: "https://broker.example"}, fixture, 1080, 1)
	if stage, local := localCandidateErrorStage(err); !local || stage != "config" {
		t.Fatalf("config failure = %T %v, stage=%q local=%t", err, err, stage, local)
	}
	if tickets.Load() != 0 {
		t.Fatalf("config failure requested %d WSS ticket(s)", tickets.Load())
	}
}

func TestPunchProbeFailureIsAGenuineDirectPathFailure(t *testing.T) {
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return nil })
	s.probeTunnel = func(context.Context, int) (int64, error) {
		return 0, errors.New("punched Reality path carried no data")
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := &candidateResult{
		relay: fixture, accessTransport: "punch",
		ctx: ctx, cancel: cancel, proxyPort: 1080,
	}

	_, err := s.startCandidate(result, client.SingBoxConfigInput{
		Relay: fixture, Mode: client.ModeProxy,
		ProxyListenAddress: "127.0.0.1", ProxyListenPort: 1080,
	})
	if stage, directPath := directPathErrorStage(err); !directPath || stage != "internet_probe" {
		t.Fatalf("punch probe failure = %T %v, stage=%q direct=%t", err, err, stage, directPath)
	}
}

func TestWSSDirectFailureFallsBackOnSameRelayAndPreservesReality(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	fixture.ClientID = "reality-client-id"
	fixture.RealityPublicKey = "reality-public-key"
	fixture.ShortID = "0102030405060708"
	fixture.ServerName = "camouflage.example.com"
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) { return 0, errors.New("direct TCP blocked") }

	type ticketCall struct {
		brokerURL string
		request   relay.WSSSessionTicketRequest
	}
	ticketCalls := make(chan ticketCall, 1)
	s.requestWSSTicket = func(_ context.Context, brokerURL string, request relay.WSSSessionTicketRequest, _, _ string) (relay.WSSSessionTicketResponse, error) {
		ticketCalls <- ticketCall{brokerURL: brokerURL, request: request}
		return successfulWSSTicket(fixture.WSSFronts[0], "opaque-ticket"), nil
	}
	bridge := newFakeWSSBridge()
	s.dialWSS = func(_ context.Context, rawURL, ticket string) (wssBridge, error) {
		if rawURL != fixture.WSSFronts[0].URL || ticket != "opaque-ticket" {
			t.Fatalf("WSS dial URL/ticket = %q/%q", rawURL, ticket)
		}
		return bridge, nil
	}
	type configCapture struct {
		path string
		body []byte
		err  error
	}
	configs := make(chan configCapture, 1)
	s.runTunnel = func(ctx context.Context, path string) error {
		body, err := os.ReadFile(path)
		configs <- configCapture{path: path, body: body, err: err}
		<-ctx.Done()
		return nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	waitWSSSignal(t, bridge.started, "WSS Serve")
	call := <-ticketCalls
	if call.brokerURL != sink.srv.URL || call.request.RelayID != fixture.ID || call.request.FrontID != fixture.WSSFronts[0].ID {
		t.Fatalf("ticket call = %+v", call)
	}
	captured := <-configs
	if captured.err != nil {
		t.Fatal(captured.err)
	}
	var generated struct {
		Outbounds []struct {
			Type       string `json:"type"`
			Server     string `json:"server"`
			ServerPort int    `json:"server_port"`
			UUID       string `json:"uuid"`
			Flow       string `json:"flow"`
			TLS        struct {
				ServerName string `json:"server_name"`
				Reality    struct {
					PublicKey string `json:"public_key"`
					ShortID   string `json:"short_id"`
				} `json:"reality"`
			} `json:"tls"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(captured.body, &generated); err != nil {
		t.Fatal(err)
	}
	outbound := generated.Outbounds[0]
	if outbound.Server != bridge.host || outbound.ServerPort != bridge.port || outbound.UUID != fixture.ClientID || outbound.Flow != fixture.Flow ||
		outbound.TLS.ServerName != fixture.ServerName || outbound.TLS.Reality.PublicKey != fixture.RealityPublicKey || outbound.TLS.Reality.ShortID != fixture.ShortID {
		t.Fatalf("WSS changed Reality config: %+v", outbound)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	waitWSSSignal(t, bridge.exited, "WSS exit")
	if bridge.closeCalls.Load() != 1 {
		t.Fatalf("bridge Close calls = %d", bridge.closeCalls.Load())
	}
	if _, err := os.Stat(captured.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate config survived cleanup: %v", err)
	}
	attempts := sink.named("relay_attempt_failed")
	if len(attempts) != 1 || attempts[0].RelayID != fixture.ID {
		t.Fatalf("direct failure health events = %+v", attempts)
	}
	succeeded := sink.named("connection_succeeded")
	if len(succeeded) != 1 || succeeded[0].Attributes["transport"] != accessTransportWSS || succeeded[0].Attributes["front_id"] != fixture.WSSFronts[0].ID {
		t.Fatalf("WSS success telemetry = %+v", succeeded)
	}
	if _, present := succeeded[0].Measurements["relay_tcp_ms"]; present {
		t.Fatalf("WSS success reported a direct TCP duration: %+v", succeeded[0].Measurements)
	}
}

func TestWSSFrontFailoverUsesExactAdvertisedFrontAndDoesNotAddRelayDamage(t *testing.T) {
	sink := newTelemetrySink(t)
	frontA := testWSSFront("front-a", testWSSFrontAURL)
	frontB := testWSSFront("front-b", testWSSFrontBURL)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10", frontA, frontB)
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) { return 0, errors.New("direct blocked") }

	var mu sync.Mutex
	var requested []relay.WSSSessionTicketRequest
	s.requestWSSTicket = func(_ context.Context, _ string, request relay.WSSSessionTicketRequest, _, _ string) (relay.WSSSessionTicketResponse, error) {
		mu.Lock()
		requested = append(requested, request)
		mu.Unlock()
		front := frontA
		if request.FrontID == frontB.ID {
			front = frontB
		}
		return successfulWSSTicket(front, "ticket-for-"+request.FrontID), nil
	}
	bridge := newFakeWSSBridge()
	var dialed []string
	s.dialWSS = func(_ context.Context, rawURL, ticket string) (wssBridge, error) {
		mu.Lock()
		dialed = append(dialed, rawURL+" "+ticket)
		call := len(dialed)
		mu.Unlock()
		if call == 1 {
			return nil, errors.New("first CDN handshake blocked")
		}
		return bridge, nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	mu.Lock()
	gotRequests := append([]relay.WSSSessionTicketRequest(nil), requested...)
	gotDials := append([]string(nil), dialed...)
	mu.Unlock()
	if !reflect.DeepEqual(gotRequests, []relay.WSSSessionTicketRequest{{RelayID: fixture.ID, FrontID: frontA.ID}, {RelayID: fixture.ID, FrontID: frontB.ID}}) {
		t.Fatalf("front-bound requests = %+v", gotRequests)
	}
	if !reflect.DeepEqual(gotDials, []string{frontA.URL + " ticket-for-front-a", frontB.URL + " ticket-for-front-b"}) {
		t.Fatalf("exact front dials = %v", gotDials)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 1 {
		t.Fatalf("WSS failure added relay-health damage: %+v", attempts)
	}
	transportFailures := sink.named("transport_failed")
	if len(transportFailures) != 1 || transportFailures[0].Attributes["front_id"] != frontA.ID {
		t.Fatalf("isolated WSS health = %+v", transportFailures)
	}
}

func TestWSSTicketURLMismatchFailsClosed(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) { return 0, errors.New("direct blocked") }
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		ticket := successfulWSSTicket(fixture.WSSFronts[0], "ticket")
		ticket.URL = testWSSFrontBURL
		return ticket, nil
	}
	var dials atomic.Int32
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
		dials.Add(1)
		return newFakeWSSBridge(), nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if dials.Load() != 0 {
		t.Fatalf("mismatched URL dialed %d times", dials.Load())
	}
	if attempts := sink.named("relay_attempt_failed"); len(attempts) != 1 {
		t.Fatalf("ticket mismatch added relay damage: %+v", attempts)
	}
	failures := sink.named("transport_failed")
	if len(failures) != 1 || failures[0].Attributes["failure_stage"] != "ticket_binding" {
		t.Fatalf("ticket binding telemetry = %+v", failures)
	}
}

func TestWSSFatalOfflineRecoveryWaitsThenRunsFreshDirectFirstLadder(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.networkRetryDelay = 2 * time.Millisecond
	var networkUp atomic.Bool
	networkUp.Store(true)
	s.checkNetworkAlive = func(context.Context, []string) bool { return networkUp.Load() }

	var sequenceMu sync.Mutex
	var sequence []string
	appendSequence := func(value string) {
		sequenceMu.Lock()
		sequence = append(sequence, value)
		sequenceMu.Unlock()
	}
	var directCalls atomic.Int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		call := directCalls.Add(1)
		appendSequence("direct-" + string(rune('0'+call)))
		return 0, errors.New("direct path blocked")
	}
	var ticketCalls atomic.Int32
	s.requestWSSTicket = func(_ context.Context, _ string, request relay.WSSSessionTicketRequest, _, _ string) (relay.WSSSessionTicketResponse, error) {
		call := ticketCalls.Add(1)
		appendSequence("ticket-" + string(rune('0'+call)))
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-"+string(rune('0'+call))), nil
	}
	first, second := newFakeWSSBridge(), newFakeWSSBridge()
	var bridgeCalls atomic.Int32
	s.dialWSS = func(_ context.Context, _ string, ticket string) (wssBridge, error) {
		call := bridgeCalls.Add(1)
		appendSequence("wss-" + string(rune('0'+call)) + "-" + ticket)
		if call == 1 {
			return first, nil
		}
		return second, nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	waitWSSSignal(t, first.started, "first WSS session")
	networkUp.Store(false)
	first.fatal <- errors.New("WSS session lost with local network")
	waitForStatus(t, s, StatusConnecting)
	time.Sleep(25 * time.Millisecond)
	if directCalls.Load() != 1 || ticketCalls.Load() != 1 {
		t.Fatalf("offline recovery started early: direct=%d tickets=%d", directCalls.Load(), ticketCalls.Load())
	}
	networkUp.Store(true)
	waitWSSSignal(t, second.started, "fresh WSS session")
	waitForStatus(t, s, StatusConnected)

	sequenceMu.Lock()
	gotSequence := append([]string(nil), sequence...)
	sequenceMu.Unlock()
	wantSequence := []string{"direct-1", "ticket-1", "wss-1-single-use-1", "direct-2", "ticket-2", "wss-2-single-use-2"}
	if !reflect.DeepEqual(gotSequence, wantSequence) {
		t.Fatalf("recovery order/ticket reuse = %v, want %v", gotSequence, wantSequence)
	}
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	waitWSSSignal(t, first.exited, "first WSS cleanup")
	waitWSSSignal(t, second.exited, "second WSS cleanup")
	transportFailures := sink.named("transport_failed")
	if len(transportFailures) != 1 || transportFailures[0].Attributes["failure_stage"] != "wss_session" {
		t.Fatalf("fatal WSS health = %+v", transportFailures)
	}
	for _, attempt := range sink.named("relay_attempt_failed") {
		if len(attempt.Measurements) == 0 {
			t.Fatalf("fatal WSS session itself damaged relay health: %+v", attempt)
		}
	}
}

func TestWSSActiveSingBoxExitIsLocalAndDoesNotMintAnotherTicket(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	var directCalls atomic.Int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		directCalls.Add(1)
		return 0, errors.New("direct blocked")
	}
	var tickets atomic.Int32
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		tickets.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-ticket"), nil
	}
	bridge := newFakeWSSBridge()
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) { return bridge, nil }
	crash := make(chan error, 1)
	s.runTunnel = func(ctx context.Context, _ string) error {
		select {
		case err := <-crash:
			return err
		case <-ctx.Done():
			return nil
		}
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	crash <- errors.New("local sing-box crashed")
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	waitWSSSignal(t, bridge.exited, "WSS cleanup after local process exit")
	if tickets.Load() != 1 || directCalls.Load() != 1 {
		t.Fatalf("local process exit restarted ladder: direct=%d tickets=%d", directCalls.Load(), tickets.Load())
	}
	if failures := sink.named("transport_failed"); len(failures) != 0 {
		t.Fatalf("local process exit was misreported as WSS transport failure: %+v", failures)
	}
	attempts := sink.named("relay_attempt_failed")
	if len(attempts) != 1 || len(attempts[0].Measurements) == 0 {
		t.Fatalf("local process exit damaged relay health: %+v", attempts)
	}
	failed := sink.named("connection_failed")
	if len(failed) != 1 || failed[0].Attributes["failure_stage"] != "tunnel_process" {
		t.Fatalf("local terminal failure telemetry = %+v", failed)
	}
}

func TestWSSHealthProbeFailureIsTransportScoped(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.healthTick = 2 * time.Millisecond
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }
	var probeCalls atomic.Int32
	s.healthProbe = func(context.Context, int) error {
		if probeCalls.Add(1) <= config.HealthFailureThreshold {
			return errors.New("CDN path blackholed Reality bytes")
		}
		return nil
	}
	var directCalls atomic.Int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		if directCalls.Add(1) == 1 {
			return 0, errors.New("initial direct path blocked")
		}
		return 1, nil
	}
	var tickets atomic.Int32
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		tickets.Add(1)
		return successfulWSSTicket(fixture.WSSFronts[0], "single-use-ticket"), nil
	}
	bridge := newFakeWSSBridge()
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) { return bridge, nil }

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.named("relay_failover")) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failovers := sink.named("relay_failover"); len(failovers) != 1 || failovers[0].Attributes["transport"] != relay.TransportDirect {
		t.Fatalf("WSS health recovery = %+v", failovers)
	}
	if tickets.Load() != 1 || directCalls.Load() != 2 {
		t.Fatalf("recovery was not fresh direct-first: direct=%d tickets=%d", directCalls.Load(), tickets.Load())
	}
	transportFailures := sink.named("transport_failed")
	if len(transportFailures) != 1 || transportFailures[0].Attributes["failure_stage"] != "wss_health_probe" {
		t.Fatalf("WSS health failure telemetry = %+v", transportFailures)
	}
	attempts := sink.named("relay_attempt_failed")
	if len(attempts) != 1 || len(attempts[0].Measurements) == 0 {
		t.Fatalf("WSS health failure damaged relay ranking: %+v", attempts)
	}
	waitWSSSignal(t, bridge.exited, "blackholed WSS cleanup")
	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestWSSFatalOfflineWaitStopsOnDisconnect(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, _ := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.networkRetryDelay = time.Millisecond
	s.checkNetworkAlive = func(context.Context, []string) bool { return false }
	var directCalls atomic.Int32
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		directCalls.Add(1)
		return 0, errors.New("direct blocked")
	}
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		return successfulWSSTicket(fixture.WSSFronts[0], "ticket"), nil
	}
	bridge := newFakeWSSBridge()
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) { return bridge, nil }

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	bridge.fatal <- errors.New("offline WSS stop")
	waitForStatus(t, s, StatusConnecting)
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	if directCalls.Load() != 1 {
		t.Fatalf("disconnect did not stop offline recovery: direct calls=%d", directCalls.Load())
	}
	if logs := logLines(s); !strings.Contains(logs, "waiting for connectivity") {
		t.Fatalf("missing offline recovery log:\n%s", logs)
	}
}
