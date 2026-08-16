package connectcore

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/client"
	"openrung/internal/relay"
)

// The typed notices exist so host UIs (the TUI) can render failover, WSS
// fallback, punch, and health-probe state without parsing log lines. These
// tests pin the emission points; presentation belongs to the host.

func TestMidSessionFailoverEmitsTypedNotices(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []relay.Descriptor{
		relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, events := newLadderService(t, func() []relay.Descriptor { return fixtures })

	crash := make(chan error, 1)
	var runs int32
	s.runTunnel = func(ctx context.Context, configPath string) error {
		if atomic.AddInt32(&runs, 1) == 1 {
			select {
			case err := <-crash:
				return err
			case <-ctx.Done():
				return nil
			}
		}
		<-ctx.Done()
		return nil
	}

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	crash <- errors.New("sing-box exited: exit status 1")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(events.noticesOf(NoticeFailoverCompleted)) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	started := events.noticesOf(NoticeFailoverStarted)
	if len(started) != 1 || started[0].FromRelayID != "a" || started[0].Reason != "tunnel process exited unexpectedly" {
		t.Fatalf("failover-started notices = %+v", started)
	}
	completed := events.noticesOf(NoticeFailoverCompleted)
	if len(completed) != 1 || completed[0].FromRelayID != "a" || completed[0].RelayID != "b" {
		t.Fatalf("failover-completed notices = %+v", completed)
	}
	if completed[0].Reason != started[0].Reason {
		t.Fatalf("completed notice lost the trigger reason: %+v", completed[0])
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestWSSFallbackEmitsTypedNoticePerFront(t *testing.T) {
	sink := newTelemetrySink(t)
	fixture := relayWithWSS("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, events := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fixture} })
	s.dialRelay = func(context.Context, string, int) (int64, error) { return 0, errors.New("direct TCP blocked") }
	s.requestWSSTicket = func(_ context.Context, _ string, request relay.WSSSessionTicketRequest, _, _ string) (relay.WSSSessionTicketResponse, error) {
		return successfulWSSTicket(fixture.WSSFronts[0], "opaque-ticket"), nil
	}
	bridge := newFakeWSSBridge()
	s.dialWSS = func(context.Context, string, string) (wssBridge, error) { return bridge, nil }

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)

	fallbacks := events.noticesOf(NoticeWSSFallback)
	if len(fallbacks) != 1 {
		t.Fatalf("WSS fallback notices = %+v", fallbacks)
	}
	notice := fallbacks[0]
	if notice.RelayID != fixture.ID || notice.FrontID != fixture.WSSFronts[0].ID {
		t.Fatalf("fallback notice names wrong relay/front: %+v", notice)
	}
	if !strings.Contains(notice.Reason, "direct TCP blocked") {
		t.Fatalf("fallback notice lost the direct failure: %+v", notice)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestWSSTicketRetryEmitsTypedNotice(t *testing.T) {
	s := New()
	sink := &testSink{}
	s.Sink = sink
	front := testWSSFront("front-a", testWSSFrontAURL)

	// Every front rate-limits the first round; the retry round succeeds.
	fronts := wssTicketBrokerFronts("")
	var calls int32
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		if int(atomic.AddInt32(&calls, 1)) <= len(fronts) {
			return relay.WSSSessionTicketResponse{}, &client.WSSTicketStatusError{StatusCode: 429, RetryAfter: 2 * time.Second}
		}
		return successfulWSSTicket(front, "opaque-ticket"), nil
	}
	s.waitWSSRetry = func(context.Context, time.Duration) error { return nil }

	conn := &connection{}
	ticket, err := s.requestWSSSessionTicket(context.Background(), conn, relay.WSSSessionTicketRequest{
		RelayID: "relay-a",
		FrontID: front.ID,
	})
	if err != nil || ticket.Ticket != "opaque-ticket" {
		t.Fatalf("ticket = %+v, err = %v", ticket, err)
	}

	retries := sink.noticesOf(NoticeWSSTicketRetry)
	if len(retries) != 1 || retries[0].RelayID != "relay-a" || retries[0].FrontID != front.ID || retries[0].Wait != 2*time.Second {
		t.Fatalf("ticket-retry notices = %+v", retries)
	}
}

func TestPunchFailureEmitsOutcomeNotice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var notices []Notice
	// Port 1 on loopback refuses immediately, so the hub coordination call
	// fails and the attempt reports its outcome without any network wait.
	est := AttemptPunch(ctx, nil, relay.Descriptor{ID: "relay-a", PunchCapable: true}, PunchOptions{
		Enabled: true,
		BaseURL: "http://127.0.0.1:1",
		Notify:  func(n Notice) { notices = append(notices, n) },
	})
	if est != nil {
		t.Fatalf("punch against a dead hub succeeded: %+v", est)
	}
	if len(notices) != 1 || notices[0].Kind != NoticePunchOutcome || notices[0].RelayID != "relay-a" {
		t.Fatalf("punch notices = %+v", notices)
	}
	if !strings.Contains(notices[0].Reason, "failed") || !strings.Contains(notices[0].Reason, "using relay hub") {
		t.Fatalf("punch outcome reason = %q", notices[0].Reason)
	}
}

func TestHealthLoopEmitsProbeNotices(t *testing.T) {
	s := New()
	sink := &testSink{}
	s.Sink = sink
	s.healthTick = time.Millisecond
	var probes int32
	s.healthProbe = func(context.Context, int) error {
		if atomic.AddInt32(&probes, 1) == 1 {
			return nil
		}
		return errors.New("probe timeout")
	}
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	failCh := make(chan error, 1)
	go s.healthLoop(context.Background(), 1080, nil, failCh)
	select {
	case <-failCh:
	case <-time.After(5 * time.Second):
		t.Fatal("health loop never reported the failover trigger")
	}

	got := sink.noticesOf(NoticeHealthProbe)
	if len(got) != 1+HealthFailureThreshold {
		t.Fatalf("health notices = %+v, want one passing sweep plus %d failures", got, HealthFailureThreshold)
	}
	if got[0].Failures != 0 || got[0].Threshold != HealthFailureThreshold {
		t.Fatalf("passing sweep notice = %+v", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i].Failures != i || got[i].Threshold != HealthFailureThreshold {
			t.Fatalf("failure notice %d = %+v", i, got[i])
		}
	}
}

// A prolonged local outage keeps the internal failure counter growing past the
// threshold (the network-alive gate keeps refusing the failover); the notice
// reads "N of threshold", so the notified count must stay capped there.
func TestHealthLoopCapsNotifiedFailuresAtThreshold(t *testing.T) {
	s := New()
	sink := &testSink{}
	s.Sink = sink
	s.healthTick = time.Millisecond
	s.healthProbe = func(context.Context, int) error { return errors.New("probe timeout") }
	// The network stays down for several over-threshold sweeps, then recovers
	// so the loop finally reports the failover trigger and exits.
	var networkChecks int32
	s.checkNetworkAlive = func(context.Context, []string) bool {
		return atomic.AddInt32(&networkChecks, 1) > 3
	}

	failCh := make(chan error, 1)
	go s.healthLoop(context.Background(), 1080, nil, failCh)
	select {
	case <-failCh:
	case <-time.After(5 * time.Second):
		t.Fatal("health loop never reported the failover trigger")
	}

	got := sink.noticesOf(NoticeHealthProbe)
	if len(got) < HealthFailureThreshold+3 {
		t.Fatalf("health notices = %d, want the below-threshold sweeps plus the outage sweeps", len(got))
	}
	for i, notice := range got {
		if notice.Failures > notice.Threshold {
			t.Fatalf("notice %d exceeds the threshold: %+v", i, notice)
		}
	}
	last := got[len(got)-1]
	if last.Failures != HealthFailureThreshold || last.Reason != "" {
		t.Fatalf("failover-trigger notice = %+v", last)
	}
}

// TestEngineTelemetryCarriesCLIPlatformLabel is the local-broker acceptance
// check for the distinct CLI platform label: with Engine.Platform set to
// PlatformCLI, every telemetry event that reaches the (loopback) broker
// carries attrs["platform"] = "cli"; the default (desktop) engine sends no
// platform attribute at all, keeping desktop telemetry byte-identical.
func TestEngineTelemetryCarriesCLIPlatformLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		platform  string
		wantLabel string
	}{
		{name: "cli", platform: string(PlatformCLI), wantLabel: "cli"},
		{name: "desktop-default", platform: "", wantLabel: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newTelemetrySink(t)
			fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
			s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
			s.Platform = brokerapi.Platform(tc.platform)

			if err := s.Connect(sink.srv.URL, "", ""); err != nil {
				t.Fatalf("connect: %v", err)
			}
			waitForStatus(t, s, StatusConnected)
			_ = s.Disconnect()
			waitForStatus(t, s, StatusDisconnected)
			waitIdle(t, s)

			events := sink.named("connection_succeeded")
			if len(events) != 1 {
				t.Fatalf("connection_succeeded = %+v", events)
			}
			if got := events[0].Attributes["platform"]; got != tc.wantLabel {
				t.Fatalf("platform attribute = %q, want %q", got, tc.wantLabel)
			}
			ended := sink.named("connection_ended")
			if len(ended) != 1 {
				t.Fatalf("connection_ended = %+v", ended)
			}
			if got := ended[0].Attributes["platform"]; got != tc.wantLabel {
				t.Fatalf("connection_ended platform attribute = %q, want %q", got, tc.wantLabel)
			}
		})
	}
}
