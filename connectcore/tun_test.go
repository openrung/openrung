package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/discovery"
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
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
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
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, configJSON []byte) error {
		tunnelStarted.Store(true)
		<-ctx.Done()
		return nil
	})

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
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
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
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
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
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.Elevation = permitElevation{}
	proxy := &fakeProxyController{supported: true}
	s.OSProxy = proxy
	store := &fakePersistence{}
	s.Persistence = store
	s.FakeIPCachePath = filepath.Join(t.TempDir(), "fakeip.db")
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}

	configs := make(chan []byte, 1)
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, config []byte) error {
		configs <- append([]byte(nil), config...)
		<-ctx.Done()
		return nil
	})

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	configJSON := <-configs

	info, ok := s.ActiveConnectionInfo()
	if !ok || info.ProxyPort != 0 {
		t.Fatalf("ActiveConnectionInfo = %+v ok=%v; want no proxy port", info, ok)
	}
	if proxy.sets != 0 {
		t.Fatalf("TUN mode set the OS proxy %d times", proxy.sets)
	}
	// TUN mode never allocates or persists a port for a listener that will not
	// exist.
	if loads, saves := store.portCalls(); loads != 0 || saves != 0 {
		t.Fatalf("TUN mode touched proxy-port persistence: %d loads, %d saves", loads, saves)
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
	cache := decoded["experimental"].(map[string]any)["cache_file"].(map[string]any)
	if cache["path"] != s.FakeIPCachePath || cache["store_fakeip"] != true {
		t.Fatalf("TUN run did not use the host's persistent DNS cache: %v", cache)
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
	s, sink := newLadderService(t, func() []brokerapi.RelayDescriptor { return []brokerapi.RelayDescriptor{fronted} })
	s.Elevation = permitElevation{}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.dialRelay = func(context.Context, string, int) (int64, error) {
		return 0, errors.New("connection refused")
	}
	s.requestWSSTicket = func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
		t.Error("TUN mode requested a WSS session ticket")
		return brokerapi.WSSTicketResponse{}, errors.New("unreachable")
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
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })

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
	go s.healthLoop(t.Context(), 0, nil, failCh, nil)
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

// Readiness in TUN mode is the kernel routing internet-bound traffic out of
// the tunnel address — there is no loopback inbound to dial, and the device
// existing is not the same as the device carrying the default path.
func TestTUNInterfaceReady(t *testing.T) {
	stubRoute := func(source net.IP, err error) {
		routeSourceIP = func(string, string) (net.IP, error) { return source, err }
	}
	original := routeSourceIP
	t.Cleanup(func() { routeSourceIP = original })

	// Before sing-box installs its routes, traffic still leaves the physical
	// interface.
	stubRoute(net.ParseIP("192.168.0.71"), nil)
	if err := tunInterfaceReady(t.Context(), 0); err == nil {
		t.Fatal("reported ready while traffic still leaves the physical interface")
	}

	stubRoute(tunnelAddressIPv4, nil)
	if err := tunInterfaceReady(t.Context(), 0); err != nil {
		t.Fatalf("tunInterfaceReady once the tunnel owns the route: %v", err)
	}

	// A v6-only host answers on the second family; the v4 lookup has no route.
	routeSourceIP = func(network, _ string) (net.IP, error) {
		if network == "udp4" {
			return nil, errors.New("no route to host")
		}
		return tunnelAddressIPv6, nil
	}
	if err := tunInterfaceReady(t.Context(), 0); err != nil {
		t.Fatalf("tunInterfaceReady on a v6-only route: %v", err)
	}

	// Every family unroutable is not readiness.
	stubRoute(nil, errors.New("no route to host"))
	if err := tunInterfaceReady(t.Context(), 0); err == nil {
		t.Fatal("reported ready with no routable family")
	}
}

// The regression this readiness check exists for: 172.19.0.1 lives in the
// range Docker carves bridge networks from, so a host with a matching bridge
// holds the tunnel address on an interface while the tunnel does not exist.
// Readiness must not fire — otherwise the direct internet probe that follows
// passes over the ordinary network and an untunneled session is published
// CONNECTED.
func TestTUNReadinessIgnoresAForeignHolderOfTheTunnelAddress(t *testing.T) {
	original := routeSourceIP
	t.Cleanup(func() { routeSourceIP = original })

	// A docker0-style bridge owns 172.19.0.1, but internet-bound traffic still
	// leaves via the LAN: exactly what the kernel reports here.
	routeSourceIP = func(string, string) (net.IP, error) {
		return net.ParseIP("192.168.0.71"), nil
	}
	if err := tunInterfaceReady(t.Context(), 0); err == nil {
		t.Fatal("a foreign interface holding the tunnel address reported the tunnel ready")
	} else if !strings.Contains(err.Error(), "192.168.0.71") {
		t.Fatalf("readiness error = %v; want the source it actually found", err)
	}
}

// The engine gives a TUN rung longer to come up than a proxy rung, because
// readiness now waits for the routes and not just a bound socket.
func TestTUNUsesTheLongerReadyTimeout(t *testing.T) {
	s := New()
	if s.readyLimit() != TunnelReadyTimeout {
		t.Fatalf("proxy ready limit = %v; want %v", s.readyLimit(), TunnelReadyTimeout)
	}
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	if s.readyLimit() != TUNTunnelReadyTimeout {
		t.Fatalf("TUN ready limit = %v; want %v", s.readyLimit(), TUNTunnelReadyTimeout)
	}
	s.tunnelReadyLimit = time.Second // an explicit override still wins
	if s.readyLimit() != time.Second {
		t.Fatalf("overridden ready limit = %v", s.readyLimit())
	}
}

// permitElevation is a host that already holds TUN privileges.
type permitElevation struct{}

func (permitElevation) Elevate(context.Context) error { return nil }

// fakeDNSController records the engine's use of the OSDNS hook. Snapshot
// returns snap, or extend(prev) when set.
type fakeDNSController struct {
	mu         sync.Mutex
	supported  bool
	snap       OSProxySnapshot
	extend     func(prev OSProxySnapshot) OSProxySnapshot
	onSet      func(snap OSProxySnapshot)
	setErr     error
	restoreErr error
	prevs      []OSProxySnapshot
	servers    []string
	setSnaps   []OSProxySnapshot
	restores   []OSProxySnapshot
}

func (f *fakeDNSController) Supported() bool { return f.supported }

func (f *fakeDNSController) Snapshot(prev OSProxySnapshot) (OSProxySnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prevs = append(f.prevs, prev)
	if f.extend != nil {
		return f.extend(prev), nil
	}
	return f.snap, nil
}

func (f *fakeDNSController) Set(server string, snap OSProxySnapshot) error {
	if f.onSet != nil {
		f.onSet(snap)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = append(f.servers, server)
	f.setSnaps = append(f.setSnaps, snap)
	return f.setErr
}

func (f *fakeDNSController) Restore(snap OSProxySnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, snap)
	return f.restoreErr
}

func (f *fakeDNSController) calls() ([]string, []OSProxySnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.servers...), append([]OSProxySnapshot(nil), f.restores...)
}

// A connected TUN session points the system resolver at the tunnel's DNS
// address, keeps the pre-tunnel snapshot persisted for crash recovery while
// connected, and restores it (and only it) on disconnect.
func TestTUNConnectPointsSystemDNSAtTunnelAndRestores(t *testing.T) {
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.Elevation = permitElevation{}
	proxy := &fakeProxyController{supported: true}
	s.OSProxy = proxy
	snap := testProxySnapshot{Server: "dns:9.9.9.9"}
	dns := &fakeDNSController{supported: true, snap: snap}
	s.OSDNS = dns
	store := &fakePersistence{}
	s.Persistence = store
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, _ []byte) error {
		<-ctx.Done()
		return nil
	})

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	servers, restores := dns.calls()
	if len(servers) != 1 || servers[0] != client.DefaultTunnelDNSAddress {
		t.Fatalf("OSDNS.Set calls = %v; want [%s]", servers, client.DefaultTunnelDNSAddress)
	}
	if len(restores) != 0 {
		t.Fatalf("restored DNS while connected: %v", restores)
	}
	if got, ok := store.LoadProxySnapshot(); !ok || got != snap {
		t.Fatalf("persisted snapshot = %v (present %v); want %v", got, ok, snap)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	_, restores = dns.calls()
	if len(restores) != 1 || restores[0] != snap {
		t.Fatalf("OSDNS restores = %v; want [%v]", restores, snap)
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("clean disconnect left the crash-recovery snapshot behind")
	}
	if proxy.sets != 0 || len(proxy.restores) != 0 {
		t.Fatalf("TUN session touched the OS proxy: %d sets, %d restores", proxy.sets, len(proxy.restores))
	}
}

// Proxy mode keeps routing through the OS proxy only; the resolver stays as
// the user configured it.
func TestProxyModeNeverTouchesSystemDNS(t *testing.T) {
	fixtures := []brokerapi.RelayDescriptor{relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	dns := &fakeDNSController{supported: true}
	s.OSDNS = dns

	if err := s.Connect("", "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if servers, restores := dns.calls(); len(servers) != 0 || len(restores) != 0 {
		t.Fatalf("proxy mode used OSDNS: sets %v, restores %v", servers, restores)
	}
}

// A failed Set may have changed some services: the snapshot is restored at
// once and, once restored, no recovery snapshot is left pending.
func TestTunnelDNSSetFailureRestoresSnapshot(t *testing.T) {
	snap := testProxySnapshot{Server: "dns:dhcp"}
	dns := &fakeDNSController{supported: true, snap: snap, setErr: errors.New("exit status 4")}
	store := &fakePersistence{}
	s := New()
	s.OSDNS = dns
	s.Persistence = store
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	conn := &connection{}

	s.applyProxy(conn, 0)

	if conn.proxySet {
		t.Fatal("a failed Set must not leave a restore pending once restored")
	}
	if _, restores := dns.calls(); len(restores) != 1 || restores[0] != snap {
		t.Fatalf("restores = %v; want [%v]", restores, snap)
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("restored snapshot still persisted")
	}

	// A recovery release in TUN mode goes through OSDNS too.
	dns.setErr = nil
	s.applyProxy(conn, 0)
	if !conn.proxySet || !s.releaseProxy(conn) || conn.proxySet {
		t.Fatal("TUN-mode release did not restore through OSDNS")
	}
}

// Crash recovery: the persisted slot holds whichever snapshot the last session
// took, so Start hands it to both hooks and clears it only when both succeed.
func TestStartRestoresLeftoverSnapshotThroughBothHooks(t *testing.T) {
	snap := testProxySnapshot{Server: "dns:9.9.9.9"}
	store := &fakePersistence{}
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatal(err)
	}
	proxy := &fakeProxyController{supported: true}
	dns := &fakeDNSController{supported: true, restoreErr: errors.New("busy")}
	s := New()
	s.OSProxy = proxy
	s.OSDNS = dns
	s.Persistence = store

	s.Start()
	if len(proxy.restores) != 1 || proxy.restores[0] != snap {
		t.Fatalf("OSProxy restores = %v; want [%v]", proxy.restores, snap)
	}
	if _, ok := store.LoadProxySnapshot(); !ok {
		t.Fatal("a failed DNS restore must keep the snapshot for the next launch")
	}

	dns.restoreErr = nil
	s.Start()
	if _, restores := dns.calls(); len(restores) != 2 || restores[1] != snap {
		t.Fatalf("OSDNS restores = %v; want the snapshot twice", restores)
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("successful restore through both hooks must clear the snapshot")
	}
}

// Every promote re-snapshots from the captured snapshot, so the hook can add a
// network service that appeared mid-session; the extended snapshot is
// persisted before Set and handed to Set, so nothing Set changes is missing
// from the recovery file.
func TestTunnelDNSResnapshotsBeforeEachSet(t *testing.T) {
	store := &fakePersistence{}
	dns := &fakeDNSController{supported: true}
	dns.extend = func(prev OSProxySnapshot) OSProxySnapshot {
		if prev == nil {
			return testProxySnapshot{Server: "wifi"}
		}
		return testProxySnapshot{Server: prev.(testProxySnapshot).Server + "+usb"}
	}
	dns.onSet = func(snap OSProxySnapshot) {
		if got, ok := store.LoadProxySnapshot(); !ok || got != snap {
			t.Errorf("Set ran with %v before it was persisted (persisted %v)", snap, got)
		}
	}
	s := New()
	s.OSDNS = dns
	s.Persistence = store
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	conn := &connection{}

	s.applyProxy(conn, 0)
	if !s.releaseProxy(conn) { // a failover gap
		t.Fatal("release failed")
	}
	s.applyProxy(conn, 0)

	dns.mu.Lock()
	defer dns.mu.Unlock()
	if len(dns.prevs) != 2 || dns.prevs[0] != nil || dns.prevs[1] != (testProxySnapshot{Server: "wifi"}) {
		t.Fatalf("Snapshot prevs = %v; want [nil wifi]", dns.prevs)
	}
	want := testProxySnapshot{Server: "wifi+usb"}
	if len(dns.setSnaps) != 2 || dns.setSnaps[1] != want || conn.snapshot != want {
		t.Fatalf("second Set got %v (conn holds %v); want %v", dns.setSnaps, conn.snapshot, want)
	}
}

// A snapshot an earlier session left unrestored holds the user's real
// settings. Until its restore succeeds a new session must not snapshot (it
// would capture the tunnel's leftover server as the user's and overwrite the
// original) or change anything.
func TestTunnelDNSWaitsForLeftoverSnapshotRestore(t *testing.T) {
	leftover := testProxySnapshot{Server: "dns:original"}
	store := &fakePersistence{}
	if err := store.SaveProxySnapshot(leftover); err != nil {
		t.Fatal(err)
	}
	dns := &fakeDNSController{supported: true, snap: testProxySnapshot{Server: "dns:tunnel"}, restoreErr: errors.New("busy")}
	s := New()
	s.OSDNS = dns
	s.Persistence = store
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.Start() // the launch-time restore fails too

	conn := &connection{}
	s.applyProxy(conn, 0)
	if got, ok := store.LoadProxySnapshot(); !ok || got != leftover {
		t.Fatalf("persisted snapshot = %v (present %v); want the leftover kept", got, ok)
	}
	if prevs, servers := len(dns.prevs), len(dns.servers); prevs != 0 || servers != 0 || conn.snapshotTaken {
		t.Fatalf("changed DNS over an unrestored snapshot: %d snapshots, %d sets", prevs, servers)
	}

	dns.restoreErr = nil
	s.applyProxy(conn, 0)
	_, restores := dns.calls()
	if len(restores) != 3 || restores[2] != leftover {
		t.Fatalf("restores = %v; want the leftover restored before the new snapshot", restores)
	}
	if servers, _ := dns.calls(); len(servers) != 1 || !conn.proxySet {
		t.Fatalf("Set calls = %v after the leftover was restored; want one", servers)
	}
	if got, _ := store.LoadProxySnapshot(); got != (testProxySnapshot{Server: "dns:tunnel"}) {
		t.Fatalf("persisted snapshot = %v; want this session's", got)
	}
}
