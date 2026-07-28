package engine

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

type recordedProbe struct {
	mu          sync.Mutex
	calls       []int
	listenHosts []string
	outcomes    map[int]relayruntime.DirectProbeResult
}

func (p *recordedProbe) probe(_ context.Context, _, _, listenHost string, port int, _ *http.Client) relayruntime.DirectProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, port)
	p.listenHosts = append(p.listenHosts, listenHost)
	if result, ok := p.outcomes[port]; ok {
		return result
	}
	return relayruntime.DirectProbeResult{Outcome: relayruntime.DirectProbeExternallyUnreachable}
}

func (p *recordedProbe) ports() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.calls...)
}

func (p *recordedProbe) hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.listenHosts...)
}

func (p *recordedProbe) setOutcomes(outcomes map[int]relayruntime.DirectProbeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcomes = outcomes
}

func newProbeEngine(log *bytes.Buffer, probe func(context.Context, string, string, string, int, *http.Client) relayruntime.DirectProbeResult) *Engine {
	eng := New(Config{}, Events{Log: log})
	eng.probeDirect = probe
	return eng
}

func TestAutomaticPortCandidateOrderAndDeduplication(t *testing.T) {
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{}}
	eng := newProbeEngine(&bytes.Buffer{}, recorder.probe)

	mode, _, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 443, 8443, 443, 8443},
	})

	if mode != ModeTunnel || port != 0 {
		t.Fatalf("resolution = (%q, %d), want tunnel with no selected port", mode, port)
	}
	if got, want := recorder.ports(), []int{443, 8443}; !equalPorts(got, want) {
		t.Fatalf("probe order = %v, want %v", got, want)
	}
}

func TestAutomaticPortCandidatesDefaultToListenPortOnly(t *testing.T) {
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{}}
	eng := newProbeEngine(&bytes.Buffer{}, recorder.probe)

	_, _, _ = eng.autoResolve(context.Background(), Config{ListenPort: 9443})

	if got, want := recorder.ports(), []int{9443}; !equalPorts(got, want) {
		t.Fatalf("generic probe order = %v, want %v", got, want)
	}
}

func TestAutomaticProbeAndPublicListenerUseSameWildcardStrategy(t *testing.T) {
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "198.51.100.7",
		},
	}}
	eng := newProbeEngine(&bytes.Buffer{}, recorder.probe)

	mode, _, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})
	if mode != ModeDirect || port != 443 {
		t.Fatalf("resolution = (%q, %d), want direct on 443", mode, port)
	}
	if got, want := recorder.hosts(), []string{automaticDirectListenHost}; !equalStrings(got, want) {
		t.Fatalf("probe listen hosts = %q, want %q", got, want)
	}

	// Both the nonce probe and ConnectionObserver must resolve the automatic
	// wildcard to one generic TCP listener. This lets Go use IPv4 on a host whose
	// IPv6 stack is disabled instead of positive-probing on IPv4 and then
	// requiring a failing tcp6 listener for the real session.
	probeAddr := relayruntime.ProbeBindAddr(automaticDirectListenHost, 443)
	listenerAddrs := relayruntime.ListenAddressesForHost(automaticDirectListenHost, 443)
	if len(listenerAddrs) != 1 || listenerAddrs[0] != probeAddr {
		t.Fatalf("automatic bind strategy: probe=%q listener=%v, want one identical wildcard", probeAddr, listenerAddrs)
	}
	if automaticDirectListenHost == directOnlyListenHost {
		t.Fatal("automatic wildcard unexpectedly changed direct-only listener semantics")
	}
}

func TestAutomaticPortCandidateValidation(t *testing.T) {
	auto := Config{
		BrokerURL:               "https://broker.example",
		Mode:                    ModeAuto,
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 0},
	}.withDefaults()
	if err := auto.validate(); err == nil || !strings.Contains(err.Error(), "automatic port candidate") {
		t.Fatalf("auto validation error = %v, want invalid candidate error", err)
	}

	// Direct-only mode does not consume automatic candidates; a stale or
	// separately managed candidate list must not override its explicit port.
	direct := auto
	direct.Mode = ModeDirect
	if err := direct.validate(); err != nil {
		t.Fatalf("direct-only validation unexpectedly consumed automatic candidates: %v", err)
	}
	if got := direct.automaticPortCandidates(); !equalPorts(got, []int{443, 0}) {
		t.Fatalf("candidate helper unexpectedly changed explicit list: %v", got)
	}
}

func TestAutomaticPortPermissionFailureFallsThroughToAlternate(t *testing.T) {
	var log bytes.Buffer
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {
			Outcome: relayruntime.DirectProbePermissionDenied,
			Err:     os.ErrPermission,
		},
		8443: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "198.51.100.8",
		},
	}}
	eng := newProbeEngine(&log, recorder.probe)

	mode, host, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})

	if mode != ModeDirect || host != "198.51.100.8" || port != 8443 {
		t.Fatalf("resolution = (%q, %q, %d), want direct on 198.51.100.8:8443", mode, host, port)
	}
	if got, want := recorder.ports(), []int{443, 8443}; !equalPorts(got, want) {
		t.Fatalf("probe order = %v, want %v", got, want)
	}
	if got := log.String(); !strings.Contains(got, "local permission denied") ||
		!strings.Contains(got, "positively reachable") {
		t.Fatalf("probe log does not distinguish permission and success:\n%s", got)
	}
}

func TestAutomaticPortExternalFailureFallsThroughToAlternate(t *testing.T) {
	var log bytes.Buffer
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
		8443: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "203.0.113.9",
		},
	}}
	eng := newProbeEngine(&log, recorder.probe)

	mode, _, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})

	if mode != ModeDirect || port != 8443 {
		t.Fatalf("resolution = (%q, %d), want direct on 8443", mode, port)
	}
	if got := log.String(); !strings.Contains(got, "externally unreachable or firewalled") ||
		strings.Contains(got, "permission denied") {
		t.Fatalf("external failure was misreported:\n%s", got)
	}
}

func TestAutomaticPortFirstSuccessStopsCandidateProbing(t *testing.T) {
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "203.0.113.10",
		},
		8443: {Outcome: relayruntime.DirectProbeReachable, ObservedHost: "203.0.113.10"},
	}}
	eng := newProbeEngine(&bytes.Buffer{}, recorder.probe)

	mode, _, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})

	if mode != ModeDirect || port != 443 {
		t.Fatalf("resolution = (%q, %d), want direct on 443", mode, port)
	}
	if got, want := recorder.ports(), []int{443}; !equalPorts(got, want) {
		t.Fatalf("probe order = %v, want %v", got, want)
	}
}

func TestAutomaticPortAllCandidatesFailSelectsTunnel(t *testing.T) {
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443:  {Outcome: relayruntime.DirectProbePortInUse, Err: errors.New("address already in use")},
		8443: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
	}}
	eng := newProbeEngine(&bytes.Buffer{}, recorder.probe)

	mode, host, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})

	if mode != ModeTunnel || host != "" || port != 0 {
		t.Fatalf("resolution = (%q, %q, %d), want tunnel with no endpoint", mode, host, port)
	}
	if got, want := recorder.ports(), []int{443, 8443}; !equalPorts(got, want) {
		t.Fatalf("probe order = %v, want %v", got, want)
	}
}

func TestAutomaticPortAllCandidatesFailRunsTunnelSession(t *testing.T) {
	_, hubAddr := startTestHub(t)
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443:  {Outcome: relayruntime.DirectProbePortInUse, Err: errors.New("address already in use")},
		8443: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
	}}
	eng := New(Config{
		BrokerURL:               "http://127.0.0.1:1",
		Mode:                    ModeAuto,
		HubAddr:                 hubAddr,
		HubPlaintext:            true,
		Label:                   "candidate-fallback-test",
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
		Identity:                testIdentity,
		DisableXray:             true,
		ConfigDir:               t.TempDir(),
	}, Events{})
	eng.probeDirect = recorder.probe

	if err := eng.Start(); err != nil {
		t.Fatalf("start auto relay: %v", err)
	}
	defer eng.Stop()
	eventually(t, 5*time.Second, "RelayHub fallback to come online", func() bool {
		status := eng.Status()
		return status.Phase == PhaseOnline && status.Transport == relay.TransportTunnel
	})
	if got, want := recorder.ports(), []int{443, 8443}; !equalPorts(got, want) {
		t.Fatalf("pre-tunnel probe order = %v, want %v", got, want)
	}
}

func TestAutomaticPortProbeAPIUnavailableFailsSafe(t *testing.T) {
	var log bytes.Buffer
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {
			Outcome: relayruntime.DirectProbeAPIUnavailable,
			Err:     errors.New("hub refused connection"),
		},
		8443: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "203.0.113.11",
		},
	}}
	eng := newProbeEngine(&log, recorder.probe)

	mode, _, port := eng.autoResolve(context.Background(), Config{
		ListenPort:              8443,
		AutomaticPortCandidates: []int{443, 8443},
	})

	if mode != ModeTunnel || port != 0 {
		t.Fatalf("resolution = (%q, %d), want fail-safe tunnel", mode, port)
	}
	if got, want := recorder.ports(), []int{443}; !equalPorts(got, want) {
		t.Fatalf("API-wide failure probe order = %v, want %v", got, want)
	}
	if got := log.String(); !strings.Contains(got, "RelayHub probe API unavailable") {
		t.Fatalf("missing probe API outcome in log:\n%s", got)
	}
}

func TestTunnelPeriodicReprobeUsesCandidateOrderAndPromotesLiveSession(t *testing.T) {
	setReprobeInterval(t, 20*time.Millisecond)
	broker := &fakeBroker{}
	server := httptest.NewServer(broker.handler())
	defer server.Close()
	hub, hubAddr := startTestHub(t)

	alternate := freePort(t)
	hubPublicPort, err := hub.Allocator.Allocate()
	if err != nil {
		t.Fatalf("inspect test hub public port: %v", err)
	}
	hub.Allocator.Release(hubPublicPort)
	for attempts := 0; alternate == hubPublicPort && attempts < 16; attempts++ {
		alternate = freePort(t)
	}
	if alternate == hubPublicPort {
		t.Fatalf("could not reserve a direct port distinct from test RelayHub port %d", hubPublicPort)
	}
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443:       {Outcome: relayruntime.DirectProbeExternallyUnreachable},
		alternate: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
	}}
	eng := New(Config{
		BrokerURL:               server.URL,
		Mode:                    ModeAuto,
		HubAddr:                 hubAddr,
		HubPlaintext:            true,
		Label:                   "periodic-promotion-test",
		ListenPort:              alternate,
		AutomaticPortCandidates: []int{443, alternate},
		Identity:                testIdentity,
		DisableXray:             true,
		ConfigDir:               t.TempDir(),
	}, Events{})
	eng.probeDirect = recorder.probe
	if err := eng.Start(); err != nil {
		t.Fatalf("start auto relay: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "initial RelayHub session", func() bool {
		status := eng.Status()
		return status.Phase == PhaseOnline && status.Transport == relay.TransportTunnel
	})

	// Make only the alternate directly reachable. The live tunnel watcher must
	// repeat 443 -> alternate, stop the tunnel, let the supervisor resolve the
	// same order again, and bring up a real direct listener/registration on the
	// selected alternate.
	recorder.setOutcomes(map[int]relayruntime.DirectProbeResult{
		443: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
		alternate: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "127.0.0.1",
		},
	})
	eventually(t, 5*time.Second, "tunnel promotion to a direct session", func() bool {
		status := eng.Status()
		return status.Phase == PhaseOnline &&
			status.Transport == relay.TransportDirect &&
			status.PublicPort == alternate
	})

	_, _, registered := broker.stats()
	if registered.PublicPort != alternate {
		t.Fatalf("promoted broker registration port = %d, want %d", registered.PublicPort, alternate)
	}
	if got := recorder.ports(); !hasPortSuffix(got, []int{443, alternate, 443, alternate}) {
		t.Fatalf("periodic promotion probes = %v, want suffix [443 %d 443 %d]", got, alternate, alternate)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(alternate)), time.Second)
	if err != nil {
		t.Fatalf("promoted direct listener is not on port %d: %v", alternate, err)
	}
	_ = conn.Close()
}

func TestSelectedAutomaticPortPropagatesThroughDirectSession(t *testing.T) {
	broker := &fakeBroker{}
	server := httptest.NewServer(broker.handler())
	defer server.Close()

	alternate := freePort(t)
	var log bytes.Buffer
	recorder := &recordedProbe{outcomes: map[int]relayruntime.DirectProbeResult{
		443: {Outcome: relayruntime.DirectProbeExternallyUnreachable},
		alternate: {
			Outcome:      relayruntime.DirectProbeReachable,
			ObservedHost: "127.0.0.1",
		},
	}}
	cfg := Config{
		BrokerURL:               server.URL,
		Mode:                    ModeAuto,
		HubAddr:                 "relayhub.invalid:9443",
		Label:                   "selected-port-test",
		ListenPort:              alternate,
		AutomaticPortCandidates: []int{443, alternate},
		Identity:                testIdentity,
		DisableXray:             true,
		ConfigDir:               t.TempDir(),
	}
	eng := New(cfg, Events{Log: &log})
	eng.probeDirect = recorder.probe
	brokerClient := &relayruntime.BrokerClient{BaseURL: server.URL}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.runSession(ctx, brokerClient) }()

	eventually(t, 5*time.Second, "selected direct port to come online", func() bool {
		status := eng.Status()
		return status.Phase == PhaseOnline && status.PublicPort == alternate
	})

	status := eng.Status()
	if status.Transport != "direct" || status.PublicHost != "127.0.0.1" || status.PublicPort != alternate {
		t.Fatalf("online status = %+v, want direct at 127.0.0.1:%d", status, alternate)
	}
	_, _, registered := broker.stats()
	if registered.PublicPort != alternate {
		t.Fatalf("broker registration port = %d, want %d", registered.PublicPort, alternate)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(alternate)), time.Second)
	if err != nil {
		t.Fatalf("selected public listener is not on port %d: %v", alternate, err)
	}
	_ = conn.Close()
	portText := strconv.Itoa(alternate)
	if got := log.String(); !strings.Contains(got, "TCP "+portText) ||
		!strings.Contains(got, net.JoinHostPort("127.0.0.1", portText)) {
		t.Fatalf("selected port missing from direct logs:\n%s", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("direct session shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("direct session did not stop")
	}
}

func TestDirectOnlyIgnoresAutomaticCandidates(t *testing.T) {
	broker := &fakeBroker{}
	server := httptest.NewServer(broker.handler())
	defer server.Close()

	directPort := freePort(t)
	eng := New(Config{
		BrokerURL:               server.URL,
		Mode:                    ModeDirect,
		Label:                   "direct-only-test",
		ListenPort:              directPort,
		AutomaticPortCandidates: []int{443, 8443},
		Identity:                testIdentity,
		DisableXray:             true,
		ConfigDir:               t.TempDir(),
	}, Events{})
	eng.probeDirect = func(context.Context, string, string, string, int, *http.Client) relayruntime.DirectProbeResult {
		t.Fatal("direct-only mode unexpectedly ran an automatic reachability probe")
		return relayruntime.DirectProbeResult{}
	}
	stubIPv6(t, "127.0.0.1", nil)
	brokerClient := &relayruntime.BrokerClient{BaseURL: server.URL}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.runSession(ctx, brokerClient) }()

	eventually(t, 5*time.Second, "direct-only session to come online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	_, _, registered := broker.stats()
	if registered.PublicPort != directPort || eng.Status().PublicPort != directPort {
		t.Fatalf("direct-only ports: registration=%d status=%d, want %d",
			registered.PublicPort, eng.Status().PublicPort, directPort)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("direct-only shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("direct-only session did not stop")
	}
}

func equalPorts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func hasPortSuffix(got, suffix []int) bool {
	if len(got) < len(suffix) {
		return false
	}
	return equalPorts(got[len(got)-len(suffix):], suffix)
}
