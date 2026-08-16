package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/discovery"
	"openrung/internal/relay"
)

// failingElevation is a host that cannot grant TUN privileges — the CLI's
// answer when it was not started under sudo.
type failingElevation struct{ calls atomic.Int64 }

func (e *failingElevation) Elevate(context.Context) error {
	e.calls.Add(1)
	return errors.New("TUN mode needs root privileges to create the tunnel device: rerun as `sudo client connect --tun`")
}

// A refused elevation must stop the connect before anything is dialed, and the
// guidance has to survive intact into lastError, which is what the TUI and the
// headless driver show.
func TestTUNConnectRefusedWithoutPrivileges(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	hook := &failingElevation{}
	s.Elevation = hook
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}

	var fetched, tunnelStarted atomic.Bool
	s.fetchRelays = func(context.Context, string, int, string, string) (discovery.Fetch, error) {
		fetched.Store(true)
		return discovery.Fetch{}, errors.New("broker must not be reached")
	}
	s.runTunnel = func(ctx context.Context, configPath string) error {
		tunnelStarted.Store(true)
		<-ctx.Done()
		return nil
	}

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	if state.LastError == nil || !strings.Contains(*state.LastError, "sudo") {
		t.Fatalf("lastError = %v; want the rerun-under-sudo guidance", state.LastError)
	}
	if hook.calls.Load() != 1 {
		t.Fatalf("Elevate calls = %d; want exactly one", hook.calls.Load())
	}
	if fetched.Load() || tunnelStarted.Load() {
		t.Fatal("a refused elevation still reached the broker or started sing-box")
	}
}

// Without an Elevation hook there is no way to know the process may create a
// TUN device, so the engine must refuse rather than let sing-box fail opaquely.
func TestTUNConnectRefusedWithoutElevationHook(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	state := waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if state.LastError == nil || !strings.Contains(*state.LastError, "elevation") {
		t.Fatalf("lastError = %v; want the missing-hook refusal", state.LastError)
	}
}

// Proxy mode is untouched by the new gate: no Elevation hook is consulted.
func TestProxyModeNeverElevates(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	hook := &failingElevation{}
	s.Elevation = hook

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if hook.calls.Load() != 0 {
		t.Fatalf("proxy mode called Elevate %d times", hook.calls.Load())
	}
}

// A TUN session never binds the loopback endpoint and never touches the OS
// proxy: pointing the system proxy at a port nothing listens on would
// blackhole the very traffic the tunnel carries.
func TestTUNConnectSkipsProxyPortAndOSProxy(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.Elevation = permitElevation{}
	proxy := &fakeProxyController{supported: true}
	s.OSProxy = proxy
	s.ResolveProxyPort = func() (ProxyPortResolution, error) {
		t.Error("TUN mode resolved the stable proxy port")
		return ProxyPortResolution{}, errors.New("unreachable")
	}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}

	var configJSON []byte
	s.writeConfig = func(data []byte) (string, error) {
		configJSON = append([]byte(nil), data...)
		return writeTempConfig(data)
	}

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	info, ok := s.ActiveConnectionInfo()
	if !ok || info.ProxyPort != 0 {
		t.Fatalf("ActiveConnectionInfo = %+v ok=%v; want no proxy port", info, ok)
	}
	if proxy.sets != 0 {
		t.Fatalf("TUN mode set the OS proxy %d times", proxy.sets)
	}

	// The generated config is the unchanged full-device TUN shape.
	var decoded map[string]any
	if err := json.Unmarshal(configJSON, &decoded); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	inbound := decoded["inbounds"].([]any)[0].(map[string]any)
	if inbound["type"] != "tun" || inbound["auto_route"] != true || inbound["strict_route"] != true {
		t.Fatalf("inbound is not the TUN shape: %+v", inbound)
	}
	if inbound["dns_mode"] != "hijack" {
		t.Fatalf("TUN inbound lost DNS hijack: %+v", inbound)
	}
	excluded := inbound["route_exclude_address"].([]any)
	if len(excluded) != 1 || excluded[0] != "127.0.0.10/32" {
		t.Fatalf("relay IP not excluded from the TUN routes: %v", excluded)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if len(proxy.restores) != 0 {
		t.Fatalf("TUN mode restored an OS proxy it never set (%d times)", len(proxy.restores))
	}
}

// The WSS bridge dials its CDN front from this process, which a full-device
// TUN would capture into the tunnel the bridge is carrying. TUN mode must stay
// on the direct failure instead of building that loop.
func TestTUNModeSkipsWSSFallback(t *testing.T) {
	fronted := relayWithWSS("a", "JP", "Tokyo", "Japan", "127.0.0.10")
	s, sink := newLadderService(t, func() []relay.Descriptor { return []relay.Descriptor{fronted} })
	s.Elevation = permitElevation{}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("connection refused")
	}
	s.requestWSSTicket = func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
		t.Error("TUN mode requested a WSS session ticket")
		return relay.WSSSessionTicketResponse{}, errors.New("unreachable")
	}

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)
	if !strings.Contains(sink.logLines(), "TUN mode cannot use them") {
		t.Fatalf("no log line explained the skipped WSS fallback:\n%s", sink.logLines())
	}
}

// The mode fixes the inbound shape, the OS-proxy policy, the probes, and the
// elevation requirement, so a session keeps the mode it started with.
func TestSetModeRefusedWhileConnected(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.SetMode(ModeTUN); err == nil {
		t.Fatal("SetMode succeeded mid-session")
	}
	if s.Mode() != ModeProxy {
		t.Fatalf("mode = %v after a refused SetMode", s.Mode())
	}
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatalf("SetMode after disconnect: %v", err)
	}
	if s.Mode() != ModeTUN {
		t.Fatalf("mode = %v; want TUN", s.Mode())
	}
}

// In TUN mode the tunnel owns the default route, so the network-alive
// reference points are reached through the very tunnel under suspicion and the
// gate could only ever answer "network down" — which would disable failover for
// good. TUN mode therefore fails over on the threshold alone.
func TestTUNHealthFailoverSkipsTheNetworkAliveGate(t *testing.T) {
	s := New()
	sink := &testSink{}
	s.Sink = sink
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.healthTick = time.Millisecond
	s.healthProbe = func(context.Context, int) error { return errors.New("probe timeout") }
	var networkChecks atomic.Int64
	s.checkNetworkAlive = func(context.Context, []string) bool {
		networkChecks.Add(1)
		return false
	}

	failCh := make(chan error, 1)
	go s.healthLoop(t.Context(), 0, nil, failCh)
	select {
	case <-failCh:
	case <-time.After(5 * time.Second):
		t.Fatal("TUN health loop never reported the failover trigger")
	}
	if networkChecks.Load() != 0 {
		t.Fatalf("TUN mode consulted the through-tunnel network-alive gate %d times", networkChecks.Load())
	}
	if strings.Contains(sink.logLines(), "network looks down") {
		t.Fatalf("TUN mode reported a local outage it cannot detect:\n%s", sink.logLines())
	}
}

// Readiness in TUN mode is the tunnel device carrying the address the
// generated config assigns it — there is no loopback inbound to dial.
func TestTUNInterfaceReady(t *testing.T) {
	original := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = original })

	loopbackOnly := []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}
	interfaceAddrs = func() ([]net.Addr, error) { return loopbackOnly, nil }
	if err := tunInterfaceReady(t.Context(), 0); err == nil {
		t.Fatal("reported ready with no tunnel interface")
	}

	interfaceAddrs = func() ([]net.Addr, error) {
		return append(loopbackOnly, &net.IPNet{IP: tunnelAddressIPv4, Mask: net.CIDRMask(30, 32)}), nil
	}
	if err := tunInterfaceReady(t.Context(), 0); err != nil {
		t.Fatalf("tunInterfaceReady with the tunnel address present: %v", err)
	}

	// The IPv6 tunnel address alone also proves the device is up.
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: tunnelAddressIPv6, Mask: net.CIDRMask(126, 128)}}, nil
	}
	if err := tunInterfaceReady(t.Context(), 0); err != nil {
		t.Fatalf("tunInterfaceReady with the IPv6 tunnel address present: %v", err)
	}
}

// permitElevation is a host that already holds TUN privileges.
type permitElevation struct{}

func (permitElevation) Elevate(context.Context) error { return nil }
