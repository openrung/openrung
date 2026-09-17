package connectcore

// Independent expectations: mobile main 53e03d9, shipping
// SingBoxConfiguration, ensureLocalTunnelPreconditions, TunnelStartupGuard,
// StartupPathVerification, TunnelPathProbe, PacketTunnel*Probe, TelemetryManager,
// ApplicationConnectionAggregator, RecentNode and OpenRungStatusStore.
// These tests deliberately do not derive expectations from the A4 vectors.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/discovery"
	"github.com/openrung/openrung/punchcore"
)

const mobileInstallID = "ABCDEF01-2345-4678-9ABC-DEF012345678"

type mobileTestRuntime struct {
	preflight func(context.Context, []byte) error
	launch    func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error)
}

func (r *mobileTestRuntime) Preflight(ctx context.Context, config []byte) error {
	if r.preflight != nil {
		return r.preflight(ctx, config)
	}
	return nil
}
func (r *mobileTestRuntime) Run(ctx context.Context, config []byte, reporter *RunTelemetry) (MobileTunnelRun, error) {
	if r.launch != nil {
		return r.launch(ctx, config, reporter)
	}
	return newMobileTestRun(PathAndroidVPN), nil
}

type mobileTestRun struct {
	done   chan error
	once   sync.Once
	ready  func(context.Context) error
	verify func(context.Context, VerificationPhase) (TunnelPathEvidence, error)
	stop   func()
	path   TunnelPath
}

func newMobileTestRun(path TunnelPath) *mobileTestRun {
	return &mobileTestRun{done: make(chan error, 1), path: path}
}
func (r *mobileTestRun) Done() <-chan error { return r.done }
func (r *mobileTestRun) finish(err error)   { r.once.Do(func() { r.done <- err; close(r.done) }) }
func (r *mobileTestRun) Stop(time.Duration) error {
	if r.stop != nil {
		r.stop()
	}
	r.finish(nil)
	return nil
}
func (r *mobileTestRun) WaitReady(ctx context.Context) error {
	if r.ready != nil {
		return r.ready(ctx)
	}
	return nil
}
func (r *mobileTestRun) VerifyPath(ctx context.Context, phase VerificationPhase) (TunnelPathEvidence, error) {
	if r.verify != nil {
		return r.verify(ctx, phase)
	}
	return TunnelPathEvidence{Path: r.path, FreshDNS: true, PinnedHTTPS: true}, nil
}
func mobileTestEngine(t *testing.T) (*Engine, *mobileTestRuntime, brokerapi.RelayDescriptor) {
	t.Helper()
	s := New()
	s.PunchEnabled = false
	s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
		return nil, punchcore.PunchResult{}, errors.New("unused punch adapter")
	}
	s.Platform = brokerapi.PlatformAndroid
	s.SocketProtector = &recordingProtector{}
	s.Elevation = permitElevation{}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	rt := &mobileTestRuntime{}
	s.Mobile = &MobileHost{InstallID: mobileInstallID, AppVersion: "0.3.9", PlatformVersion: "35", Runtime: rt, Settings: func(context.Context) (MobileTunnelSettings, error) {
		return MobileTunnelSettings{MTU: 1400, LogLevel: "warn", RouteFindProcess: true, ClashAPI: true}, nil
	}}
	r := relayWithWSS("relay-a", "KR", "Seoul", "Korea", "192.0.2.1")
	s.fetchRelays = func(ctx context.Context, url string, limit int, id, session string) (discovery.Fetch, error) {
		return discovery.Fetch{BrokerURL: url, Response: listOf(r)}, nil
	}
	s.dialRelay = func(context.Context, string, int) (int64, error) { return 1, nil }
	s.lookupGeo = func(context.Context, *http.Client) map[string]string { return nil }
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }
	store, err := clienttelemetry.NewOutbox(t.TempDir(), "openrung_telemetry_outbox.jsonl", func(ctx context.Context, url string, events []clienttelemetry.Event) error {
		return (clienttelemetry.HTTPClient{BaseURL: url, HTTP: s.brokerHTTPClient(), AppVersion: s.appVersion(), Platform: s.Platform, PlatformVersion: s.platformVersion()}).Send(ctx, events)
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Mobile.Outbox = store
	t.Cleanup(func() { s.Stop(); store.Close() })
	return s, rt, r
}
func mobileTestConnection() *connection { return &connection{} }

func TestMobilePreflightBlocksRemoteFailureAndTickets(t *testing.T) {
	for _, kind := range []string{"settings", "ipv4", "ipv6", "doh", "split", "log", "direct graph", "bridge graph", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, rt, r := mobileTestEngine(t)
			var dialed, tickets, graphs int
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.Mobile.Settings = func(context.Context) (MobileTunnelSettings, error) {
				v := MobileTunnelSettings{}
				switch kind {
				case "settings":
					return v, errors.New("VPN permission revoked")
				case "ipv4":
					v.TunnelIPv4Address = "172.19.0.3/30"
				case "ipv6":
					v.TunnelIPv6Address = "not-ipv6"
				case "doh":
					v.DNSServers = []string{"resolver.example"}
				case "split":
					v.SplitTunnel = &client.SplitTunnelRules{BypassCountries: []string{"xx"}}
				case "log":
					v.LogLevel = "unknown"
				}
				return v, nil
			}
			rt.preflight = func(context.Context, []byte) error {
				graphs++
				if kind == "cancel" {
					cancel()
					return nil
				}
				if kind == "direct graph" || (kind == "bridge graph" && graphs == 2) {
					return errors.New("libbox checkConfig refused")
				}
				return nil
			}
			s.dialRelay = func(context.Context, string, int) (int64, error) {
				dialed++
				return 0, errors.New("connection refused")
			}
			s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
				tickets++
				return brokerapi.WSSTicketResponse{}, errors.New("unexpected")
			}
			_, err := s.attemptCandidate(ctx, mobileTestConnection(), r, 0, 1)
			if _, local := localCandidateErrorStage(err); !local && !(kind == "cancel" && errors.Is(err, context.Canceled)) {
				t.Fatalf("want local error or cancellation: %v", err)
			}
			if dialed != 0 || tickets != 0 {
				t.Fatalf("invalid local setup dialed %d, minted %d", dialed, tickets)
			}
		})
	}
}

func TestMobileCandidateConfigurationParityAndRefresh(t *testing.T) {
	// Shipping binding uses DoH, warn, process accounting, custom TUN addresses,
	// probe priority pins ahead of LAN/country bypass, and protected bridge sockets.
	for _, transport := range []string{"direct", "punch", "wss"} {
		t.Run(transport, func(t *testing.T) {
			s, rt, r := mobileTestEngine(t)
			var calls int
			s.Mobile.Settings = func(context.Context) (MobileTunnelSettings, error) {
				calls++
				return MobileTunnelSettings{
					TunnelIPv4Address: "172.20.0.1/30", TunnelIPv6Address: "fd00::1/126", MTU: 1400,
					LogLevel: "warn", RouteFindProcess: true, ClashAPI: true, ProbeDomainSuffixes: []string{"probe.openrung.org", "cp.cloudflare.com"},
					SplitTunnel: &client.SplitTunnelRules{BypassLAN: true, BypassCountries: []string{"cn"}, ExcludedPackages: []string{"com.example.bypass"}, RuleSetDirectory: "/rules"},
				}, nil
			}
			var generated []byte
			rt.launch = func(_ context.Context, b []byte, _ *RunTelemetry) (MobileTunnelRun, error) {
				generated = append([]byte(nil), b...)
				return newMobileTestRun(PathAndroidVPN), nil
			}
			if transport == "punch" {
				s.PunchEnabled = true
				r.PunchCapable = true
				r.PunchEndpoint = "https://punch.example"
				s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
					return &PunchPath{Bridge: newFakeWSSBridge(), BridgeHost: "127.0.0.1", BridgePort: 43123, PeerIP: "198.51.100.1"}, punchcore.PunchResult{}, nil
				}
			}
			if transport == "wss" {
				s.dialRelay = func(context.Context, string, int) (int64, error) { return 0, errors.New("connection refused") }
				s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
					return successfulWSSTicket(r.WSSFronts[0], "ticket"), nil
				}
				s.dialWSS = func(context.Context, string, string) (wssBridge, error) { return newFakeWSSBridge(), nil }
			}
			res, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer res.teardown()
			if res.accessTransport != transport {
				t.Fatalf("path=%s", res.accessTransport)
			}
			var got map[string]any
			if err := json.Unmarshal(generated, &got); err != nil {
				t.Fatal(err)
			}
			inbound := got["inbounds"].([]any)[0].(map[string]any)
			if inbound["type"] != "tun" || inbound["mtu"] != float64(1400) || !reflect.DeepEqual(inbound["address"], []any{"172.20.0.1/30", "fd00::1/126"}) {
				t.Fatalf("TUN: %v", inbound)
			}
			if !reflect.DeepEqual(inbound["exclude_package"], []any{"com.example.bypass"}) {
				t.Fatalf("packages: %v", inbound)
			}
			if got["log"].(map[string]any)["level"] != "warn" || got["route"].(map[string]any)["find_process"] != true {
				t.Fatal("lost logging/accounting")
			}
			if !strings.Contains(string(generated), "experimental") || !strings.Contains(string(generated), "disable_cache") || !strings.Contains(string(generated), "geosite-cn") {
				t.Fatal("lost mobile DNS/split/accounting shape")
			}
			routeRules := got["route"].(map[string]any)["rules"].([]any)
			pinned := false
			for _, raw := range routeRules {
				rule := raw.(map[string]any)
				if rule["outbound"] == "proxy" && rule["domain_suffix"] != nil {
					pinned = true
				}
				if rule["outbound"] == "direct" && !pinned {
					t.Fatal("split bypass precedes probe route pin")
				}
			}
			if !pinned {
				t.Fatal("missing probe route pin")
			}
			dnsRules := got["dns"].(map[string]any)["rules"].([]any)
			for index, raw := range dnsRules[:4] {
				rule := raw.(map[string]any)
				if rule["domain_suffix"] == nil || ((index == 0 || index == 3) && rule["disable_cache"] != true) {
					t.Fatal("probe DNS chain lost priority or freshness")
				}
			}
			outbound := got["outbounds"].([]any)[0].(map[string]any)
			if outbound["uuid"] != r.ClientID {
				t.Fatal("host overwrote relay credentials")
			}
			if transport != "direct" {
				if outbound["server"] != "127.0.0.1" || outbound["server_port"] != float64(43123) {
					t.Fatal("lost engine bridge")
				}
				if _, ok := inbound["route_exclude_address"]; ok {
					t.Fatal("protected bridge excluded public routes")
				}
			}
			if transport == "wss" && calls != 2 {
				t.Fatalf("WSS did not refresh settings: %d", calls)
			}
			res.teardown()
			s.Mobile.Settings = func(context.Context) (MobileTunnelSettings, error) {
				return MobileTunnelSettings{}, errors.New("effective settings revoked")
			}
			if _, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 2); err == nil {
				t.Fatal("retry did not refresh settings")
			}
		})
	}
}

func TestMobileVerificationRejectsPhysicalAndIncompleteEvidence(t *testing.T) {
	for _, platform := range []brokerapi.Platform{brokerapi.PlatformAndroid, brokerapi.PlatformIOS} {
		for _, evidence := range []TunnelPathEvidence{{}, {Path: "physical", FreshDNS: true, PinnedHTTPS: true}, {Path: PathAndroidVPN, FreshDNS: true}, {Path: PathIOSProviderTunnel, PinnedHTTPS: true}} {
			t.Run(string(platform)+string(evidence.Path), func(t *testing.T) {
				s, rt, r := mobileTestEngine(t)
				s.Platform = platform
				rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
					run := newMobileTestRun("")
					run.verify = func(context.Context, VerificationPhase) (TunnelPathEvidence, error) { return evidence, nil }
					return run, nil
				}
				var tickets int
				s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
					tickets++
					return brokerapi.WSSTicketResponse{}, errors.New("unexpected")
				}
				_, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 1)
				if _, local := localCandidateErrorStage(err); !local {
					t.Fatalf("physical/incomplete result accepted: %v", err)
				}
				if tickets != 0 {
					t.Fatal("local probe minted ticket")
				}
			})
		}
	}
}

func TestMobileVerificationFailureClassificationAndStoppedTie(t *testing.T) {
	for _, kind := range []string{"dns_probe", "internet_probe", "local", "invalid stage", "stop tie", "cancel", "launch"} {
		t.Run(kind, func(t *testing.T) {
			s, rt, r := mobileTestEngine(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
				if kind == "launch" {
					return nil, errors.New("libbox start failed")
				}
				run := newMobileTestRun(PathAndroidVPN)
				run.verify = func(context.Context, VerificationPhase) (TunnelPathEvidence, error) {
					switch kind {
					case "local":
						return TunnelPathEvidence{}, errors.New("VPN Network unavailable")
					case "stop tie":
						run.finish(errors.New("libbox died"))
					case "cancel":
						cancel()
					}
					return TunnelPathEvidence{}, &RemotePathError{Stage: kind, Err: errors.New("timeout")}
				}
				return run, nil
			}
			_, err := s.attemptDirectCandidate(ctx, mobileTestConnection(), r, 0, 1)
			if kind == "dns_probe" || kind == "internet_probe" {
				if stage, ok := directPathErrorStage(err); !ok || stage != kind {
					t.Fatalf("stage %q: %v", stage, err)
				}
			} else if kind == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			} else if _, ok := localCandidateErrorStage(err); !ok {
				t.Fatalf("local failure became remote: %v", err)
			}
		})
	}
}

func TestMobileCancellationDuringReadinessAndVerification(t *testing.T) {
	for _, phase := range []string{"ready", "probe"} {
		t.Run(phase, func(t *testing.T) {
			s, rt, _ := mobileTestEngine(t)
			broker := newTelemetrySink(t)
			entered, exited := make(chan struct{}), make(chan struct{})
			var stopped atomic.Bool
			rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
				r := newMobileTestRun(PathAndroidVPN)
				r.stop = func() { stopped.Store(true) }
				wait := func(ctx context.Context) error { close(entered); <-ctx.Done(); close(exited); return ctx.Err() }
				if phase == "ready" {
					r.ready = wait
				} else {
					r.verify = func(ctx context.Context, _ VerificationPhase) (TunnelPathEvidence, error) {
						return TunnelPathEvidence{}, wait(ctx)
					}
				}
				return r, nil
			}
			if err := s.Connect(broker.srv.URL, "", ""); err != nil {
				t.Fatal(err)
			}
			waitWSSSignal(t, entered, phase)
			if err := s.Shutdown(100 * time.Millisecond); err != nil {
				t.Fatal(err)
			}
			waitWSSSignal(t, exited, "cancelled hook")
			if s.State().Status != StatusDisconnected || !stopped.Load() {
				t.Fatal("cancel failed teardown")
			}
			if len(broker.named("relay_attempt_failed")) != 0 {
				t.Fatal("cancellation penalized relay")
			}
		})
	}
}

func TestMobileReadinessMustPrecedeVerification(t *testing.T) {
	s, rt, r := mobileTestEngine(t)
	var probed bool
	rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
		run := newMobileTestRun(PathAndroidVPN)
		run.ready = func(context.Context) error { return errors.New("TUN not published") }
		run.verify = func(context.Context, VerificationPhase) (TunnelPathEvidence, error) {
			probed = true
			return TunnelPathEvidence{Path: PathAndroidVPN, FreshDNS: true, PinnedHTTPS: true}, nil
		}
		return run, nil
	}
	if _, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 1); err == nil || probed {
		t.Fatal("HTTP result substituted for TUN readiness")
	}
}

func TestMobileIdentityTelemetryOwnershipAndRestart(t *testing.T) {
	s, rt, _ := mobileTestEngine(t)
	before := map[string]string{"HOME": os.Getenv("HOME"), "XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME")}
	directory := t.TempDir()
	var mu sync.Mutex
	var received []clienttelemetry.Event
	var offline atomic.Bool
	offline.Store(true)
	send := func(ctx context.Context, _ string, events []clienttelemetry.Event) error {
		if offline.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		mu.Lock()
		received = append(received, events...)
		mu.Unlock()
		return nil
	}
	store, err := clienttelemetry.NewOutbox(directory, "openrung_telemetry_outbox.jsonl", send)
	if err != nil {
		t.Fatal(err)
	}
	s.Mobile.Outbox = store
	legacy := clienttelemetry.Event{SchemaVersion: 1, EventID: "old-event", Event: "connection_ended", OccurredAt: time.Now(), ClientID: mobileInstallID, SessionID: "old-session"}
	if store.EnqueueBatch([]clienttelemetry.Event{legacy}) != 1 {
		t.Fatal("legacy copy not durable")
	}
	s.Mobile.Attributes = func() map[string]string {
		return map[string]string{"device_model": "test", "engine": "fake", "app_version": "wrong"}
	}
	reporters := make(chan *RunTelemetry, 4)
	rt.launch = func(_ context.Context, _ []byte, p *RunTelemetry) (MobileTunnelRun, error) {
		reporters <- p
		p.UpdateTraffic(100, 200)
		p.RecordApplicationConnections("com.example.app", 123, 100001)
		r := newMobileTestRun(PathAndroidVPN)
		r.stop = func() { p.UpdateTraffic(90, 250); p.RecordApplicationConnections("com.example.app", 123, 2) }
		return r, nil
	}
	s.fetchRelays = func(_ context.Context, url string, _ int, id, session string) (discovery.Fetch, error) {
		if id != mobileInstallID || session == "" {
			t.Errorf("discovery identity %q/%q", id, session)
		}
		return discovery.Fetch{BrokerURL: url, Response: listOf(relayWithWSS("r", "KR", "Seoul", "Korea", "192.0.2.1"))}, nil
	}
	if err := s.Connect("http://127.0.0.1:1", "", ""); err != nil {
		t.Fatal(err)
	}
	first := <-reporters
	waitForStatus(t, s, StatusConnected)
	if got, _ := s.InstallID(); got != mobileInstallID {
		t.Fatalf("RN identity %q", got)
	}
	if opts := s.identityForDirectory(); opts.ClientID != mobileInstallID || opts.AppVersion != "0.3.9" || opts.PlatformVersion != "35" {
		t.Fatalf("directory identity %+v", opts)
	}
	started := time.Now()
	if err := s.Shutdown(40 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded flush=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("terminal flush exceeded budget")
	}
	if first.UpdateTraffic(999, 999) || first.RecordApplicationConnections("stale", 1, 1) {
		t.Fatal("retired run mutated telemetry")
	}
	if store.PendingCount() == 0 {
		t.Fatal("lost pending telemetry")
	}
	// Engine borrows the same owner; it must not close the store at Shutdown.
	if store.EnqueueBatch([]clienttelemetry.Event{legacy}) != 1 {
		t.Fatal("engine closed borrowed store")
	}
	store.Close()
	offline.Store(false)
	nextStore, err := clienttelemetry.NewOutbox(directory, "openrung_telemetry_outbox.jsonl", send)
	if err != nil {
		t.Fatal(err)
	}
	defer nextStore.Close()
	next := New()
	next.Platform = brokerapi.PlatformAndroid
	next.Mobile = &MobileHost{InstallID: mobileInstallID, AppVersion: "0.3.9", PlatformVersion: "35", Outbox: nextStore}
	mgr := next.newManager("http://127.0.0.1:1")
	if err := mgr.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var ended bool
	var flows int64
	var legacyFound bool
	for _, e := range received {
		if e.ClientID != mobileInstallID {
			t.Fatalf("identity changed: %+v", e)
		}
		if e.SessionID == "old-session" {
			legacyFound = true
			continue
		}
		if e.Event == "application_connection" {
			flows += e.Measurements["connection_count"]
			if len(e.Attributes) != 0 {
				t.Fatal("application row not reduced")
			}
			continue
		}
		if e.Attributes["engine"] != "connectcore" || e.Attributes["engine_version"] != "0.6.3" || e.Attributes["platform"] != "android" || e.Attributes["app_version"] != "0.3.9" || e.Attributes["device_model"] != "test" {
			t.Fatalf("release metadata %+v", e.Attributes)
		}
		if e.Event == "connection_ended" {
			ended = true
			if e.Measurements["bytes_sent"] != 100 || e.Measurements["bytes_received"] != 250 {
				t.Fatalf("final counters %+v", e.Measurements)
			}
		}
	}
	if !ended || !legacyFound || flows != 100003 {
		t.Fatalf("restart missing events: end=%v legacy=%v flows=%d", ended, legacyFound, flows)
	}
	for k, v := range before {
		if os.Getenv(k) != v {
			t.Fatalf("changed %s", k)
		}
	}
}

func TestMobileStatusAcrossRecoverySwitchAndTeardown(t *testing.T) {
	s, rt, a := mobileTestEngine(t)
	broker := newTelemetrySink(t)
	sink := &testSink{}
	s.Sink = sink
	a.Label = "Alpha"
	b := a
	b.ID = "relay-b"
	b.Label = "Beta"
	b.NodeClass = brokerapi.NodeClassVolunteer
	var fetched atomic.Int32
	s.fetchRelays = func(_ context.Context, url string, _ int, _, _ string) (discovery.Fetch, error) {
		if fetched.Add(1) == 1 {
			return discovery.Fetch{BrokerURL: url, Response: listOf(a)}, nil
		}
		return discovery.Fetch{BrokerURL: url, Response: listOf(b)}, nil
	}
	runs := make(chan *mobileTestRun, 3)
	reporters := make(chan *RunTelemetry, 3)
	var settings atomic.Int32
	s.Mobile.Settings = func(context.Context) (MobileTunnelSettings, error) {
		return MobileTunnelSettings{MTU: 1400 + int(settings.Add(1))}, nil
	}
	rt.launch = func(_ context.Context, _ []byte, p *RunTelemetry) (MobileTunnelRun, error) {
		r := newMobileTestRun(PathAndroidVPN)
		runs <- r
		reporters <- p
		return r, nil
	}
	if err := s.Connect(broker.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	firstRun, firstReporter := <-runs, <-reporters
	first := waitForStatus(t, s, StatusConnected)
	if first.Details.RelayName != "Alpha" || first.Details.RelayClass != brokerapi.NodeClassFoundation || first.Details.SessionID == "" {
		t.Fatalf("initial details %+v", first)
	}
	firstRun.finish(errors.New("engine exited"))
	secondRun, secondReporter := <-runs, <-reporters
	_ = secondRun
	deadline := time.Now().Add(3 * time.Second)
	for s.State().Status != StatusConnected || s.State().Details.RelayID != b.ID {
		if time.Now().After(deadline) {
			t.Fatal("recovery did not promote")
		}
		time.Sleep(time.Millisecond)
	}
	second := s.State()
	if second.Details.SessionID != first.Details.SessionID || second.Details.RelayClass != brokerapi.NodeClassVolunteer || second.Details.RelayName != "Beta" {
		t.Fatalf("recovery details %+v", second.Details)
	}
	if firstReporter.UpdateTraffic(999, 999) {
		t.Fatal("old recovery run owns telemetry")
	}
	if !secondReporter.UpdateTraffic(10, 20) {
		t.Fatal("current run lost telemetry")
	}
	if settings.Load() != 2 {
		t.Fatal("recovery reused persisted settings snapshot")
	}
	if len(second.Recents) != 2 || second.Recents[0].RelayID != b.ID || second.Recents[1].RelayID != a.ID {
		t.Fatalf("same-country pinned recents %+v", second.Recents)
	}
	if err := s.Connect(broker.srv.URL, "", b.ID); err != nil {
		t.Fatal(err)
	}
	<-runs
	<-reporters
	third := waitForStatus(t, s, StatusConnected)
	if third.Details.SessionID == second.Details.SessionID || third.Details.RelayID != b.ID {
		t.Fatal("switch mixed session identities")
	}
	if secondReporter.RecordApplicationConnections("stale", 1, 1) {
		t.Fatal("switch retained old reporter")
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	for _, state := range sink.statesSnapshot() {
		if state.Details == nil {
			t.Fatal("mobile event lacks atomic snapshot")
		}
		if state.Status != StatusConnected && (state.Details.RelayID != "" || state.Details.RelayName != "" || state.Details.RelayClass != "") {
			t.Fatalf("stale relay in %s: %+v", state.Status, state.Details)
		}
		if (state.Status == StatusDisconnected || state.Status == StatusFailed || state.Status == StatusPreparing) && state.Details.SessionID != "" {
			t.Fatalf("stale session in %s", state.Status)
		}
	}
	// The saved event is immutable despite successor promotions and teardown.
	if first.Details.RelayID != a.ID || first.Details.RelayName != "Alpha" {
		t.Fatal("retained event mutated")
	}
}

func TestMobileHealthUsesCurrentRunAndLocalFailuresDoNotRecover(t *testing.T) {
	for _, platform := range []brokerapi.Platform{brokerapi.PlatformAndroid, brokerapi.PlatformIOS} {
		t.Run(string(platform), func(t *testing.T) {
			s, rt, _ := mobileTestEngine(t)
			s.Platform = platform
			s.healthTick = time.Millisecond
			broker := newTelemetrySink(t)
			health := make(chan struct{}, 1)
			var launches atomic.Int32
			rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
				launches.Add(1)
				path := PathAndroidVPN
				if platform == brokerapi.PlatformIOS {
					path = PathIOSProviderTunnel
				}
				r := newMobileTestRun(path)
				r.verify = func(_ context.Context, phase VerificationPhase) (TunnelPathEvidence, error) {
					if phase == VerificationHealth {
						health <- struct{}{}
						return TunnelPathEvidence{}, errors.New("provider/VPN network unavailable")
					}
					return TunnelPathEvidence{Path: path, FreshDNS: true, PinnedHTTPS: true}, nil
				}
				return r, nil
			}
			if err := s.Connect(broker.srv.URL, "", ""); err != nil {
				t.Fatal(err)
			}
			select {
			case <-health:
			case <-time.After(time.Second):
				t.Fatal("health hook not used")
			}
			waitForStatus(t, s, StatusFailed)
			waitIdle(t, s)
			if launches.Load() != 1 || len(broker.named("relay_attempt_failed")) != 0 {
				t.Fatal("local platform health failure blamed relay or started recovery")
			}
		})
	}
}

func TestMobileFailedLaunchRetiresReporterAndAllowsCleanRetry(t *testing.T) {
	s, rt, r := mobileTestEngine(t)
	var previous *RunTelemetry
	var attempts int
	rt.launch = func(_ context.Context, _ []byte, p *RunTelemetry) (MobileTunnelRun, error) {
		attempts++
		if attempts == 1 {
			previous = p
			return nil, errors.New("platform TUN refused")
		}
		if previous.UpdateTraffic(100, 100) {
			t.Error("failed launch retained reporter")
		}
		return newMobileTestRun(PathAndroidVPN), nil
	}
	if _, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 1); err == nil {
		t.Fatal("failed launch passed")
	}
	res, err := s.attemptCandidate(t.Context(), mobileTestConnection(), r, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	res.teardown()
}

func TestMobileHeartbeatMetadataAndTraffic(t *testing.T) {
	s, _, _ := mobileTestEngine(t)
	broker := newTelemetrySink(t)
	var epoch atomic.Int32
	s.Mobile.Attributes = func() map[string]string {
		return map[string]string{"network_transport": []string{"wifi", "cellular"}[epoch.Load()], "platform": "spoofed"}
	}
	mgr := s.newManager(broker.srv.URL)
	if _, err := mgr.BeginSession(); err != nil {
		t.Fatal(err)
	}
	mgr.MarkConnected("relay")
	mgr.UpdateTraffic(20, 30)
	mgr.UpdateTraffic(10, 25)
	for i := int32(0); i < 2; i++ {
		epoch.Store(i)
		if err := mgr.Heartbeat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	events := broker.named("session_heartbeat")
	if len(events) != 2 {
		t.Fatalf("heartbeats %d", len(events))
	}
	for i, e := range events {
		if e.ClientID != mobileInstallID || e.Attributes["platform"] != "android" || e.Attributes["engine"] != "connectcore" || e.Attributes["network_transport"] != []string{"wifi", "cellular"}[i] || e.Measurements["bytes_sent"] != 20 || e.Measurements["bytes_received"] != 30 {
			t.Fatalf("heartbeat %+v", e)
		}
	}
}

func TestMobileMissingConfigurationFailsBeforeDiscovery(t *testing.T) {
	for _, kind := range []string{"identity", "runtime", "settings", "protector", "mode", "platform", "dual runtime", "dual outbox", "missing outbox", "missing punch"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := mobileTestEngine(t)
			switch kind {
			case "identity":
				s.Mobile.InstallID = ""
			case "runtime":
				s.Mobile.Runtime = nil
			case "settings":
				s.Mobile.Settings = nil
			case "protector":
				s.SocketProtector = nil
			case "mode":
				if err := s.SetMode(ModeProxy); err != nil {
					t.Fatal(err)
				}
			case "platform":
				s.Platform = PlatformCLI
			case "dual runtime":
				s.TunnelRuntime = &recordingRuntime{}
			case "dual outbox":
				s.TelemetryOutboxDirectory = t.TempDir()
			case "missing outbox":
				s.Mobile.Outbox = nil
			case "missing punch":
				s.PunchEstablisher = nil
			}
			var fetched atomic.Bool
			s.fetchRelays = func(context.Context, string, int, string, string) (discovery.Fetch, error) {
				fetched.Store(true)
				return discovery.Fetch{}, errors.New("unexpected")
			}
			if err := s.Connect("http://127.0.0.1:1", "", ""); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, s, StatusFailed)
			waitIdle(t, s)
			if fetched.Load() {
				t.Fatal("invalid host reached discovery")
			}
		})
	}
}

func TestMobileWireIdentityForDiscoveryTicketsAndTelemetry(t *testing.T) {
	s, _, relay := mobileTestEngine(t)
	s.fetchRelays = nil
	s.requestWSSTicket = nil
	var mu sync.Mutex
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(listOf(relay))
			return
		}
		if strings.Contains(r.URL.Path, "wss") {
			_ = json.NewEncoder(w).Encode(successfulWSSTicket(relay.WSSFronts[0], "test-ticket"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	mgr := s.newManager(server.URL)
	session, err := mgr.BeginSession()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.sessionID = session.ID
	s.mu.Unlock()
	if _, err := s.relayFetcher()(t.Context(), server.URL, 5, mgr.ClientID(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wssTicketRequester()(t.Context(), server.URL, brokerapi.WSSTicketRequest{RelayID: relay.ID, FrontID: relay.WSSFronts[0].ID}, mgr.ClientID(), session.ID); err != nil {
		t.Fatal(err)
	}
	mgr.Record("connection_attempted", "", nil, nil)
	if err := mgr.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 3 {
		t.Fatalf("wire requests=%d", len(headers))
	}
	for _, h := range headers {
		if h.Get("X-OpenRung-Client-ID") != mobileInstallID || h.Get("X-OpenRung-Session-ID") != session.ID {
			t.Fatalf("wire identity %+v", h)
		}
		if h.Get("X-OpenRung-App-Version") != "0.3.9" || h.Get("X-OpenRung-Android-API") != "35" {
			t.Fatalf("app version missing: %+v", h)
		}
	}
}

func TestMobileBorrowedOutboxKeepsSingleLockAndLegacyCopy(t *testing.T) {
	s, _, _ := mobileTestEngine(t)
	dir := t.TempDir()
	send := func(context.Context, string, []clienttelemetry.Event) error { return nil }
	owner, _ := clienttelemetry.NewOutbox(dir, "mobile.jsonl", send)
	defer owner.Close()
	event := clienttelemetry.Event{SchemaVersion: 1, EventID: "migration", Event: "connection_ended", OccurredAt: time.Now(), ClientID: mobileInstallID, SessionID: "legacy"}
	if owner.EnqueueBatch([]clienttelemetry.Event{event}) != 1 {
		t.Fatal("owner failed migration")
	}
	competitor, _ := clienttelemetry.NewOutbox(dir, "mobile.jsonl", send)
	defer competitor.Close()
	if competitor.EnqueueBatch([]clienttelemetry.Event{event}) != -1 {
		t.Fatal("competing lock confirmed migration; caller would delete only copy")
	}
	s.Mobile.Outbox = owner
	for i := 0; i < 2; i++ {
		m := s.newManager("http://127.0.0.1:1")
		if _, err := m.BeginSession(); err != nil {
			t.Fatal(err)
		}
		m.Record("connection_attempted", "", nil, nil)
	}
	if owner.PendingCount() != 3 {
		t.Fatal("engine managers competed for a second outbox lock")
	}
	if _, _, err := competitor.FlushNextBatch(t.Context(), "http://127.0.0.1:1"); !errors.Is(err, clienttelemetry.ErrOutboxUnavailable) {
		t.Fatalf("competitor acquired lock: %v", err)
	}
}

func TestMobileStateRemainsAtomicDuringSlowTeardown(t *testing.T) {
	s, rt, _ := mobileTestEngine(t)
	broker := newTelemetrySink(t)
	stopping, release := make(chan struct{}), make(chan struct{})
	rt.launch = func(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error) {
		r := newMobileTestRun(PathAndroidVPN)
		r.stop = func() { close(stopping); <-release }
		return r, nil
	}
	if err := s.Connect(broker.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	connected := waitForStatus(t, s, StatusConnected)
	shutdown := make(chan error, 1)
	go func() { shutdown <- s.Shutdown(time.Second) }()
	waitWSSSignal(t, stopping, "teardown")
	snapshot := s.State()
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Details, connected.Details) || snapshot.Status != connected.Status {
		t.Fatalf("partial status while resources released: %+v", snapshot)
	}
	if s.State().Details.SessionID != "" || s.State().Details.RelayID != "" {
		t.Fatal("terminal status retained identity")
	}
}

func TestMobileHealthCadenceMatchesShippingTrafficBudget(t *testing.T) {
	base := 30 * time.Second
	p := mobileProbeCadence{base: base, allowance: base}
	if !p.due(base, 10, 10, true, 0, false) {
		t.Fatal("first tick must probe")
	}
	p.healthy()
	if p.allowance != 60*time.Second {
		t.Fatal("healthy probe did not double allowance")
	}
	if p.due(base, 20, 30, true, 0, false) {
		t.Fatal("healthy tunneled downlink should save a probe")
	}
	if p.allowance != 120*time.Second {
		t.Fatal("downlink did not double allowance")
	}
	if p.due(base, 20, 30, true, 0, false) {
		t.Fatal("idle tunnel spent allowance early")
	}
	if !p.due(base, 21, 30, true, 0, false) {
		t.Fatal("send without reply must bypass allowance")
	}
	p.failed()
	if p.allowance != base || p.remaining != 0 {
		t.Fatal("remote failure did not reset allowance")
	}
	if !p.due(base, 30, 100, true, 1, false) {
		t.Fatal("traffic masked consecutive failure")
	}
	for range 10 {
		p.healthy()
	}
	if p.allowance != 5*time.Minute {
		t.Fatal("healthy backoff exceeded cap")
	}
	if !p.due(base, 40, 200, true, 0, true) {
		t.Fatal("network epoch kick masked by counters")
	}
}

func TestMobileStopDuringLaunchAndRetry(t *testing.T) {
	for _, lateSuccess := range []bool{false, true} {
		t.Run(map[bool]string{false: "launch abort", true: "cancel raced launched handle"}[lateSuccess], func(t *testing.T) {
			s, rt, _ := mobileTestEngine(t)
			broker := newTelemetrySink(t)
			entered := make(chan *RunTelemetry, 1)
			var attempts atomic.Int32
			var stopped atomic.Bool
			rt.launch = func(ctx context.Context, _ []byte, p *RunTelemetry) (MobileTunnelRun, error) {
				if attempts.Add(1) == 1 {
					entered <- p
					<-ctx.Done()
					if !lateSuccess {
						return nil, ctx.Err()
					}
					r := newMobileTestRun(PathAndroidVPN)
					r.stop = func() { stopped.Store(true) }
					return r, nil
				}
				return newMobileTestRun(PathAndroidVPN), nil
			}
			if err := s.Connect(broker.srv.URL, "", ""); err != nil {
				t.Fatal(err)
			}
			previous := <-entered
			if err := s.Shutdown(time.Second); err != nil {
				t.Fatal(err)
			}
			if previous.UpdateTraffic(1, 1) {
				t.Fatal("aborted launch retained reporter")
			}
			if lateSuccess && !stopped.Load() {
				t.Fatal("cancelled launch leaked returned run")
			}
			if err := s.Connect(broker.srv.URL, "", ""); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, s, StatusConnected)
			if err := s.Shutdown(time.Second); err != nil {
				t.Fatal(err)
			}
			if len(broker.named("connection_succeeded")) != 1 {
				t.Fatal("cancelled launch promoted or retry failed")
			}
		})
	}
}

func TestMobileReducedCountsSurviveUnavailableStoreFallback(t *testing.T) {
	s, _, _ := mobileTestEngine(t)
	dir := t.TempDir()
	send := func(context.Context, string, []clienttelemetry.Event) error { return nil }
	owner, _ := clienttelemetry.NewOutbox(dir, "outbox.jsonl", send)
	defer owner.Close()
	owner.PendingCount()
	unavailable, _ := clienttelemetry.NewOutbox(dir, "outbox.jsonl", send)
	defer unavailable.Close()
	s.Mobile.Outbox = unavailable
	var sentMu sync.Mutex
	var batches int
	var total int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Events []clienttelemetry.Event }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var count int64
		for _, e := range body.Events {
			count += e.Measurements["connection_count"]
		}
		if count > 100000 {
			t.Errorf("broker flow budget exceeded: %d", count)
		}
		sentMu.Lock()
		batches++
		total += count
		sentMu.Unlock()
		w.WriteHeader(204)
	}))
	defer server.Close()
	mgr := s.newManager(server.URL)
	if _, err := mgr.BeginSession(); err != nil {
		t.Fatal(err)
	}
	mgr.RecordApplicationConnections("relay", "com.example.app", 123, 100001)
	if err := mgr.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	sentMu.Lock()
	defer sentMu.Unlock()
	if batches != 2 || total != 100001 {
		t.Fatalf("fallback batches=%d flows=%d", batches, total)
	}
}

func TestMobileTelemetryFollowsWinningDiscoveryFront(t *testing.T) {
	s, _, relay := mobileTestEngine(t)
	winner := newTelemetrySink(t)
	s.fetchRelays = func(context.Context, string, int, string, string) (discovery.Fetch, error) {
		return discovery.Fetch{BrokerURL: winner.srv.URL, Response: listOf(relay)}, nil
	}
	if err := s.Connect("http://127.0.0.1:1", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	if len(winner.named("connection_attempted")) != 1 || len(winner.named("connection_ended")) != 1 {
		t.Fatal("mobile backlog did not follow winning front")
	}
}

// Both shipping awaitTunnelHealthFailure implementations terminate immediately
// unless isGenuineRemoteDataPathFailure recognizes the native error. An adapter
// must translate that classification before crossing the Go boundary.
func TestMobileHealthClassificationMatchesShippingThreshold(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantCalls int
		local     bool
		reset     bool
		cancel    bool
	}{
		{name: "unclassified exception", err: errors.New("native exception"), wantCalls: 1, local: true},
		{name: "bare inner timeout", err: context.DeadlineExceeded, wantCalls: 1, local: true},
		{name: "unknown stage", err: &RemotePathError{Stage: "dns_prob", Err: context.DeadlineExceeded}, wantCalls: 1, local: true},
		{name: "classified DNS timeout", err: &RemotePathError{Stage: "dns_probe", Err: context.DeadlineExceeded}, wantCalls: 3},
		{name: "classified HTTPS timeout", err: &RemotePathError{Stage: "internet_probe", Err: context.DeadlineExceeded}, wantCalls: 3},
		{name: "success resets threshold", err: &RemotePathError{Stage: "dns_probe", Err: context.DeadlineExceeded}, wantCalls: 6, reset: true},
		{name: "cancellation", err: context.Canceled, wantCalls: 1, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := mobileTestEngine(t)
			s.healthTick = time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			calls := 0
			run := newMobileTestRun(PathAndroidVPN)
			run.verify = func(_ context.Context, phase VerificationPhase) (TunnelPathEvidence, error) {
				if phase != VerificationHealth {
					t.Fatal("wrong verification phase")
				}
				calls++
				if tc.cancel {
					cancel()
				}
				if tc.reset && calls == 3 {
					return TunnelPathEvidence{Path: PathAndroidVPN, FreshDNS: true, PinnedHTTPS: true}, nil
				}
				return TunnelPathEvidence{}, tc.err
			}
			failures := make(chan error, 1)
			s.healthLoopWithProbe(ctx, 0, nil, failures, nil, func(ctx context.Context, _ int) error {
				_, err := s.verifyMobilePath(ctx, &candidateResult{mobileRun: run}, VerificationHealth)
				return err
			}, nil)
			if calls != tc.wantCalls {
				t.Fatalf("probe count %d, want %d", calls, tc.wantCalls)
			}
			select {
			case err := <-failures:
				_, local := localCandidateErrorStage(err)
				if tc.cancel || local != tc.local {
					t.Fatalf("unexpected terminal error: %v", err)
				}
			default:
				if !tc.cancel {
					t.Fatal("health loop did not report failure")
				}
			}
		})
	}
}

func TestMobileReadinessTimeoutMatchesDesktopAndPreservesCancellation(t *testing.T) {
	for _, outcome := range []string{"readiness budget", "parent cancelled", "parent deadline"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, _ := mobileTestEngine(t)
			s.tunnelReadyLimit = time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch outcome {
			case "parent cancelled":
				cancel()
			case "parent deadline":
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer deadlineCancel()
			}
			run := newMobileTestRun(PathAndroidVPN)
			joined := make(chan struct{})
			run.ready = func(ctx context.Context) error {
				defer close(joined)
				<-ctx.Done()
				return ctx.Err()
			}
			_, err := s.awaitMobileReady(ctx, &candidateResult{mobileRun: run, runDone: run.Done()})
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("readiness callback did not observe cancellation")
			}
			if outcome != "readiness budget" {
				if !errors.Is(err, ctx.Err()) {
					t.Fatalf("lost parent context error: %v", err)
				}
				return
			}
			s.tunnelReady = func(context.Context, int) error { return errors.New("not ready") }
			_, desktopErr := s.awaitTunnelReady(t.Context(), &candidateResult{}, 0)
			if err == nil || err.Error() != desktopErr.Error() || clienttelemetry.ClassifyError(err) != clienttelemetry.ClassifyError(desktopErr) {
				t.Fatalf("mobile timeout %v differs from desktop %v", err, desktopErr)
			}
		})
	}
}
