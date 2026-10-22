// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"
)

const familyAccessHost = "2001:db8:1::10"

// recordServers replaces the tunnel runtime with one that records the
// (server, uuid) of every started tunnel, and returns the reader.
func recordServers(e *accessEngine) func() []string {
	var mu sync.Mutex
	var runs []string
	e.TunnelRuntime = runFuncRuntime(func(ctx context.Context, configJSON []byte) error {
		var config struct {
			Outbounds []struct {
				Server string `json:"server"`
				UUID   string `json:"uuid"`
			} `json:"outbounds"`
		}
		_ = json.Unmarshal(configJSON, &config)
		for _, outbound := range config.Outbounds {
			if outbound.UUID != "" {
				mu.Lock()
				runs = append(runs, outbound.Server+" "+outbound.UUID)
				mu.Unlock()
			}
		}
		<-ctx.Done()
		return nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), runs...)
	}
}

// recordDials replaces the relay dialer with one that fails every host in
// fail and records every host it was asked to dial.
func recordDials(e *accessEngine, fail map[string]error) func() []string {
	var mu sync.Mutex
	var dials []string
	e.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		mu.Lock()
		dials = append(dials, host)
		mu.Unlock()
		if err, ok := fail[host]; ok {
			return 0, err
		}
		return 1, nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dials...)
	}
}

// accessOverIPv6 makes the fake broker name relay-a by an IPv6 access
// address in the access answer, as broker access_host substitution does.
func accessOverIPv6(b *fakeAccessBroker) {
	b.mutateAccess = func(d *brokerapi.RelayDescriptor) {
		if d.ID == "relay-a" {
			d.PublicHost = familyAccessHost
		}
	}
}

func connectFamily(t *testing.T, e *accessEngine, target RelayTarget) {
	t.Helper()
	if err := e.ConnectTarget("https://configured.example", target); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, e.Engine, StatusConnected)
	t.Cleanup(func() { _ = e.Disconnect(); waitIdle(t, e.Engine) })
}

// Without IPv6 the access answer's IPv6 endpoint is replaced by the relay's
// public endpoint before anything is dialled; the lease credential stays.
func TestNoIPv6UsesThePublicEndpointWithTheLease(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	accessOverIPv6(broker)
	e := newAccessEngine(t, broker, relays...)
	e.ipv6Probe = func() ipv6State { return ipv6Unavailable }
	servers := recordServers(e)
	dials := recordDials(e, nil)

	connectFamily(t, e, RelayTarget{RelayID: "relay-a"})
	runs := servers()
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "203.0.113.5 lease-relay-a") {
		t.Fatalf("tunnels = %v, want the public endpoint with the relay-a lease", runs)
	}
	for _, host := range dials() {
		if host == familyAccessHost {
			t.Fatalf("dialled the IPv6 access address without IPv6: %v", dials())
		}
	}
	if !hasLog(e.sink, "using the public IPv4 endpoint for 1 relay") {
		t.Fatal("missing the no-IPv6 rewrite log line")
	}
}

// With IPv6 (or unknown) the access address is dialled first; when it does
// not answer, the same relay is retried once on its public endpoint with the
// same lease, and no other relay is involved.
func TestIPv6ConnectFailureRetriesTheSameRelayOnItsPublicEndpoint(t *testing.T) {
	for name, state := range map[string]ipv6State{"available": ipv6Available, "unknown": ipv6Unknown} {
		t.Run(name, func(t *testing.T) {
			relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")}
			broker := newFakeAccessBroker(t, time.Now, relays...)
			accessOverIPv6(broker)
			e := newAccessEngine(t, broker, relays...)
			e.ipv6Probe = func() ipv6State { return state }
			servers := recordServers(e)
			dials := recordDials(e, map[string]error{familyAccessHost: syscall.ENETUNREACH})

			connectFamily(t, e, RelayTarget{RelayID: "relay-a"})
			if got := dials(); len(got) != 2 || got[0] != familyAccessHost || got[1] != "203.0.113.5" {
				t.Fatalf("dials = %v, want the access address then the public endpoint", got)
			}
			if runs := servers(); len(runs) != 1 || !strings.HasPrefix(runs[0], "203.0.113.5 lease-relay-a") {
				t.Fatalf("tunnels = %v, want the public endpoint with the relay-a lease", runs)
			}
			if !hasLog(e.sink, "did not answer over IPv6") {
				t.Fatal("missing the IPv6 retry log line")
			}
		})
	}
}

// A working IPv6 access address is used as the answer names it.
func TestWorkingIPv6KeepsTheAccessAddress(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	accessOverIPv6(broker)
	e := newAccessEngine(t, broker, relays...)
	e.ipv6Probe = func() ipv6State { return ipv6Available }
	servers := recordServers(e)
	dials := recordDials(e, nil)

	connectFamily(t, e, RelayTarget{RelayID: "relay-a"})
	if runs := servers(); len(runs) != 1 || !strings.HasPrefix(runs[0], familyAccessHost+" lease-relay-a") {
		t.Fatalf("tunnels = %v, want the access address with the relay-a lease", runs)
	}
	for _, host := range dials() {
		if host != familyAccessHost {
			t.Fatalf("dialled %s although the access address answered: %v", host, dials())
		}
	}
}

// Without IPv6, a relay the public list itself names by IPv6 has no other
// endpoint: it is kept but moved behind the others.
func TestNoIPv6MovesIPv6OnlyRelaysLast(t *testing.T) {
	v6 := publicRelay("relay-v6", "KR")
	v6.PublicHost = "2001:db8:2::1"
	v4 := publicRelay("relay-b", "DE")
	relays := []brokerapi.RelayDescriptor{v6, v4}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.Key = nil // public list only
	e.ipv6Probe = func() ipv6State { return ipv6Unavailable }
	servers := recordServers(e)
	_ = recordDials(e, nil)

	connectFamily(t, e, RelayTarget{})
	if runs := servers(); len(runs) != 1 || !strings.HasSuffix(runs[0], "shared-relay-b") {
		t.Fatalf("tunnels = %v, want relay-b first", runs)
	}
	if !hasLog(e.sink, "trying 1 IPv6-only relay(s) last") {
		t.Fatal("missing the reorder log line")
	}
}

func TestAdaptToAddressFamilyLeavesListsAloneUnlessIPv6IsKnownMissing(t *testing.T) {
	v6 := publicRelay("relay-a", "FI")
	v6.PublicHost = familyAccessHost
	cands := []brokerapi.RelayDescriptor{v6, publicRelay("relay-b", "DE")}
	conn := &connection{}
	conn.rememberPublicEndpoints(listOf(publicRelay("relay-a", "FI")))
	for _, state := range []ipv6State{ipv6Available, ipv6Unknown} {
		s := New()
		s.ipv6Probe = func() ipv6State { return state }
		out := s.adaptToAddressFamily(conn, cands)
		if len(out) != 2 || out[0].PublicHost != familyAccessHost || out[1].ID != "relay-b" {
			t.Fatalf("state %d changed the list: %+v", state, out)
		}
	}
	s := New()
	s.ipv6Probe = func() ipv6State { return ipv6Unavailable }
	out := s.adaptToAddressFamily(conn, cands)
	if len(out) != 2 || out[0].ID != "relay-a" || out[0].PublicHost != "203.0.113.5" || out[0].ClientID != v6.ClientID {
		t.Fatalf("no-IPv6 rewrite = %+v", out)
	}
	if cands[0].PublicHost != familyAccessHost {
		t.Fatal("the rewrite mutated the caller's list")
	}
}

func TestRememberPublicEndpointsSkipsIPv6Hosts(t *testing.T) {
	v6 := publicRelay("relay-v6", "KR")
	v6.PublicHost = "2001:db8:2::1"
	conn := &connection{}
	conn.rememberPublicEndpoints(listOf(v6, publicRelay("relay-b", "DE")))
	if _, ok := conn.publicEndpoints["relay-v6"]; ok {
		t.Fatal("an IPv6 public host was recorded as a fallback endpoint")
	}
	if endpoint := conn.publicEndpoints["relay-b"]; endpoint.host != "203.0.113.5" || endpoint.port != 443 {
		t.Fatalf("relay-b endpoint = %+v", endpoint)
	}
	if _, ok := conn.publicEndpointFor(publicRelay("relay-b", "DE")); ok {
		t.Fatal("an IPv4 candidate was offered a fallback")
	}
}

func TestClassifyIPv6RouteSource(t *testing.T) {
	for name, tc := range map[string]struct {
		source net.IP
		err    error
		want   ipv6State
	}{
		"no route":           {nil, errors.New("connect: network is unreachable"), ipv6Unavailable},
		"global source":      {net.ParseIP("2001:db8:ffff::5"), nil, ipv6Available},
		"unique-local only":  {net.ParseIP("fd8a::5"), nil, ipv6Unavailable},
		"link-local only":    {net.ParseIP("fe80::5"), nil, ipv6Unavailable},
		"our own tunnel v6":  {tunnelAddressIPv6, nil, ipv6Unknown},
		"our own tunnel v4":  {tunnelAddressIPv4, nil, ipv6Unknown},
		"unexpected v4 addr": {net.ParseIP("198.51.100.7"), nil, ipv6Unavailable},
	} {
		if got := classifyIPv6RouteSource(tc.source, tc.err); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

// A mobile host's report decides; without one a mobile engine stays unknown
// (the platform VPN may own the route table).
func TestIPv6AvailabilityPrefersTheHostReport(t *testing.T) {
	s := New()
	s.Mobile = &MobileHost{}
	if got := s.ipv6Availability(); got != ipv6Unknown {
		t.Fatalf("mobile without a report = %d, want unknown", got)
	}
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "wifi", IPv6Known: true, HasIPv6: false})
	if got := s.ipv6Availability(); got != ipv6Unavailable {
		t.Fatalf("reported no IPv6 = %d", got)
	}
	s.UpdateNetworkState(NetworkState{Up: true, Fingerprint: "cell", IPv6Known: true, HasIPv6: true})
	if got := s.ipv6Availability(); got != ipv6Available {
		t.Fatalf("reported IPv6 = %d", got)
	}
}

func hasLog(sink *testSink, fragment string) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, line := range sink.logs {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

// A socket the host refused to protect is a local failure: it is reported as
// one and never retried on the public endpoint (where a network error would
// overwrite it and count against the relay).
func TestSocketProtectionFailureIsNotRetried(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	accessOverIPv6(broker)
	e := newAccessEngine(t, broker, relays...)
	e.ipv6Probe = func() ipv6State { return ipv6Available }
	dials := recordDials(e, map[string]error{familyAccessHost: fmt.Errorf("dial: %w", wsscore.ErrSocketProtectionFailed)})

	if err := e.ConnectTarget("https://configured.example", RelayTarget{RelayID: "relay-a"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the connect to fail", func() bool { return e.State().Status == StatusFailed })
	if got := dials(); len(got) != 1 || got[0] != familyAccessHost {
		t.Fatalf("dials = %v, want only the access address", got)
	}
	if !hasLog(e.sink, "local VPN setup failed") {
		t.Fatal("the socket-protection failure was not reported as local")
	}
	_ = e.Disconnect()
	waitIdle(t, e.Engine)
}

// A recovery that reached a leased relay only by IPv6 (its public endpoint
// unknown) retries it at the endpoint the public fallback's fresh list
// reveals, on the leased credential, before the shared credentials take over.
func TestPublicFallbackRetriesLeasedRelayAtANewlyLearnedEndpoint(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")}
	broker := newFakeAccessBroker(t, time.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.ipv6Probe = func() ipv6State { return ipv6Available }
	servers := recordServers(e)
	dials := recordDials(e, map[string]error{familyAccessHost: syscall.ENETUNREACH})

	leased := publicRelay("relay-a", "FI")
	leased.PublicHost = familyAccessHost
	leased.ClientID = "lease-relay-a"
	leased.WSSFronts = []brokerapi.RelayWSSFront{{ID: "front-1", URL: "wss://cdn.example/api/v1/wss-bridge"}}
	conn := &connection{lease: accessLease{expiresAt: time.Now().Add(time.Hour), lifetime: time.Hour}}
	port := freeLoopbackPort(t)

	res, _, ok, err := e.runPublicFallback(context.Background(), conn, broker.srv.URL, RelayTarget{}, []brokerapi.RelayDescriptor{leased}, port, "", errors.New("every leased relay failed"))
	if !ok || err != nil || res == nil {
		t.Fatalf("fallback = ok %v, err %v, res %v; want the leased retry to connect", ok, err, res)
	}
	defer res.teardown()
	if res.relay.PublicHost != "203.0.113.5" || res.relay.ClientID != "lease-relay-a" || len(res.relay.WSSFronts) != 0 {
		t.Fatalf("retried candidate = %+v, want the public endpoint with the lease and no WSS fronts", res.relay)
	}
	if !conn.lease.valid() {
		t.Fatal("the lease was dropped although the leased credential connected")
	}
	if runs := servers(); len(runs) != 1 || runs[0] != "203.0.113.5 lease-relay-a" {
		t.Fatalf("tunnels = %v", runs)
	}
	for _, host := range dials() {
		if host == familyAccessHost {
			t.Fatal("the fallback dialled the IPv6 address again")
		}
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestLeasedEndpointRetriesComparesEndpointsNotIDs(t *testing.T) {
	current := relayEndpoint{host: "203.0.113.5", port: 443}
	stale := relayEndpoint{host: "198.51.100.9", port: 443}
	cand := func(host string) brokerapi.RelayDescriptor {
		c := publicRelay("relay-a", "FI")
		c.PublicHost = host
		c.WSSFronts = []brokerapi.RelayWSSFront{{ID: "front-1", URL: "wss://cdn.example/api/v1/wss-bridge"}}
		return c
	}
	for name, tc := range map[string]struct {
		tried     brokerapi.RelayDescriptor
		before    map[string]relayEndpoint
		wantRetry bool
	}{
		"ipv6, endpoint unknown before":        {cand(familyAccessHost), nil, true},
		"ipv6, endpoint changed since cache":   {cand(familyAccessHost), map[string]relayEndpoint{"relay-a": stale}, true},
		"ipv6, endpoint unchanged":             {cand(familyAccessHost), map[string]relayEndpoint{"relay-a": current}, false},
		"rewritten from a stale cache":         {cand(stale.host), map[string]relayEndpoint{"relay-a": stale}, true},
		"answer named the current endpoint":    {cand(current.host), nil, false},
		"answer named its own ipv4, now moved": {cand("192.0.2.44"), map[string]relayEndpoint{"relay-a": stale}, false},
	} {
		t.Run(name, func(t *testing.T) {
			conn := &connection{publicEndpoints: map[string]relayEndpoint{"relay-a": current}}
			retry := conn.leasedEndpointRetries([]brokerapi.RelayDescriptor{tc.tried}, tc.before)
			if !tc.wantRetry {
				if len(retry) != 0 {
					t.Fatalf("retried %+v", retry)
				}
				return
			}
			if len(retry) != 1 || retry[0].PublicHost != current.host || retry[0].PublicPort != current.port ||
				retry[0].ClientID != tc.tried.ClientID || len(retry[0].WSSFronts) != 0 {
				t.Fatalf("retry = %+v, want relay-a at %s with its credential and no WSS fronts", retry, current.host)
			}
		})
	}
}

// A recovery that fell back to a cached public endpoint which has since
// changed (the relay ID did not) retries the relay at the endpoint the fresh
// list carries, on the leased credential.
func TestPublicFallbackRetriesAChangedEndpointForAKnownRelay(t *testing.T) {
	relays := []brokerapi.RelayDescriptor{publicRelay("relay-a", "FI")} // now at 203.0.113.5
	broker := newFakeAccessBroker(t, time.Now, relays...)
	e := newAccessEngine(t, broker, relays...)
	e.ipv6Probe = func() ipv6State { return ipv6Available }
	servers := recordServers(e)
	_ = recordDials(e, map[string]error{familyAccessHost: syscall.ENETUNREACH, "198.51.100.9": syscall.ETIMEDOUT})

	leased := publicRelay("relay-a", "FI")
	leased.PublicHost = familyAccessHost
	leased.ClientID = "lease-relay-a"
	conn := &connection{
		lease:           accessLease{expiresAt: time.Now().Add(time.Hour), lifetime: time.Hour},
		publicEndpoints: map[string]relayEndpoint{"relay-a": {host: "198.51.100.9", port: 443}}, // stale cache
	}
	res, _, ok, err := e.runPublicFallback(context.Background(), conn, broker.srv.URL, RelayTarget{}, []brokerapi.RelayDescriptor{leased}, freeLoopbackPort(t), "", errors.New("every leased relay failed"))
	if !ok || err != nil || res == nil {
		t.Fatalf("fallback = ok %v, err %v; want the changed endpoint retried", ok, err)
	}
	defer res.teardown()
	if res.relay.PublicHost != "203.0.113.5" || res.relay.ClientID != "lease-relay-a" || !conn.lease.valid() {
		t.Fatalf("connected %+v (lease valid %v), want relay-a at its current endpoint on the lease", res.relay, conn.lease.valid())
	}
	if runs := servers(); len(runs) != 1 || runs[0] != "203.0.113.5 lease-relay-a" {
		t.Fatalf("tunnels = %v", runs)
	}
}
