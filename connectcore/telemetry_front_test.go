package connectcore

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/discovery"
)

// Desktop must drain both pre-discovery events and a failed upload backlog to
// the verified winner, then switch again when automatic recovery finds a new
// front. Exercise the in-memory queue and desktop's persistent outbox.
func TestDesktopTelemetryFollowsDiscoveryAndRecoveryWinners(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		name := "memory"
		if persistent {
			name = "persistent"
		}
		t.Run(name, func(t *testing.T) {
			relay := relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")
			s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{relay} })
			if persistent {
				s.TelemetryOutboxDirectory = t.TempDir()
			}
			first, second := newTelemetrySink(t), newTelemetrySink(t)
			s.SocketProtector = &recordingProtector{}
			baseTransport := s.brokerHTTPClient().Transport
			s.brokerHTTPClient().Transport = discoveryOrderTransport(func(r *http.Request) (*http.Response, error) {
				wantSNI := "http://"+r.URL.Host == first.srv.URL
				if brokerapi.AzureSNIRequested(r.Context()) != wantSNI && r.URL.Path == "/api/v1/telemetry/events" && ("http://"+r.URL.Host == first.srv.URL || "http://"+r.URL.Host == second.srv.URL) {
					t.Errorf("telemetry retained wrong Azure mode for %s", r.URL.Host)
				}
				return baseTransport.RoundTrip(r)
			})
			var recovering atomic.Bool
			var fetches atomic.Int32
			s.fetchRelays = func(context.Context, string, int, string, string) (discovery.Fetch, error) {
				winner := first.srv.URL
				if fetches.Add(1) > 1 {
					winner = second.srv.URL
				}
				return discovery.Fetch{BrokerURL: winner, Response: listOf(relay), AzureSNI: winner == first.srv.URL}, nil
			}
			s.healthTick = 5 * time.Millisecond
			s.healthProbe = func(context.Context, int) error {
				if recovering.Load() && fetches.Load() < 2 {
					return errors.New("probe timeout")
				}
				return nil
			}
			s.checkNetworkAlive = func(context.Context, []string) bool { return true }
			t.Cleanup(func() {
				_ = s.Shutdown(time.Second)
				if s.outbox != nil {
					s.outbox.Close()
				}
			})
			// The configured front is unreachable. Discovery's mocked verified
			// response selects a different front without changing the primary.
			if err := s.Connect("http://127.0.0.1:1", "", "a"); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, s, StatusConnected)
			waitEvent := func(sink *telemetrySink, event string) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if len(sink.named(event)) == 1 {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatalf("%s did not reach expected front", event)
			}
			waitEvent(first, "connection_succeeded")
			if len(first.named("connection_attempted")) != 1 {
				t.Fatal("pre-discovery backlog did not follow winner")
			}

			s.mu.Lock()
			mgr := s.conn.mgr
			s.mu.Unlock()
			if err := mgr.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitEvent(first, "session_heartbeat")

			// Strand an event on the first winner before triggering recovery.
			first.holdUntilEvent("never-release")
			mgr.Record("heartbeat", "a", nil, nil)
			if err := mgr.Flush(t.Context()); err == nil {
				t.Fatal("first winner should reject uploads")
			}
			recovering.Store(true)
			waitEvent(second, "relay_failover")
			waitEvent(second, "heartbeat")
			// A caller carrying the old mode must not override the new winner.
			if err := mgr.Heartbeat(brokerapi.WithAzureSNI(t.Context(), true)); err != nil {
				t.Fatal(err)
			}
			waitEvent(second, "session_heartbeat")
			if err := s.Shutdown(time.Second); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, s)
			waitEvent(second, "connection_ended")
			if len(first.named("heartbeat")) != 0 || len(first.named("connection_ended")) != 0 {
				t.Fatal("recovery events reached old front")
			}
			initial := first.named("connection_attempted")[0]
			terminal := second.named("connection_ended")[0]
			if initial.ClientID != terminal.ClientID || initial.SessionID != terminal.SessionID {
				t.Fatal("front switch changed session identity")
			}
			if persistent && s.outbox.PendingCount() != 0 {
				t.Fatal("persistent backlog not drained")
			}
		})
	}
}
