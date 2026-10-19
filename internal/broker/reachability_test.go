// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"openrung/internal/relay"
)

// stubProber answers every probe with its current err and records the
// endpoints it was asked about.
type stubProber struct {
	mu        sync.Mutex
	err       error
	endpoints []string
}

func (p *stubProber) Probe(_ context.Context, host string, port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpoints = append(p.endpoints, net.JoinHostPort(host, strconv.Itoa(port)))
	return p.err
}

func (p *stubProber) set(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *stubProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.endpoints)
}

// probeServer serves the relay routes and the inventory over an in-memory
// store with the given prober and re-probe interval.
func probeServer(t *testing.T, prober RelayProber, interval time.Duration) http.Handler {
	t.Helper()
	return NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), APIToken: testAPIToken, RelayProber: prober, RelayProbeInterval: interval})
}

func registerOn(t *testing.T, server http.Handler, req relay.RegisterRequest) (relay.Descriptor, map[string]json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var registration relay.RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	return registration.Descriptor, fields
}

func heartbeatOn(t *testing.T, server http.Handler, desc relay.Descriptor) {
	t.Helper()
	body, _ := json.Marshal(relay.HeartbeatRequest{OK: true, LeaseToken: desc.LeaseToken})
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/relays/"+desc.ID+"/heartbeat", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// listedIDs returns the relay IDs the public list currently serves.
func listedIDs(t *testing.T, server http.Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp relay.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(resp.Relays))
	for i, desc := range resp.Relays {
		ids[i] = desc.ID
	}
	return ids
}

// inventoryReachability returns the inventory's reachable value for relay
// id as its JSON text ("true", "false" or "null").
func inventoryReachability(t *testing.T, server http.Handler, id string) string {
	t.Helper()
	rec := getInventory(t, server)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Relays []map[string]json.RawMessage `json:"relays"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, entry := range envelope.Relays {
		if strings.Trim(string(entry["id"]), `"`) == id {
			return string(entry["reachable"])
		}
	}
	t.Fatalf("relay %s absent from inventory: %s", id, rec.Body.String())
	return ""
}

// registerRaw posts a registration and returns the recorder, for the
// refusals registerOn would fail on.
func registerRaw(t *testing.T, server http.Handler, req relay.RegisterRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/relays/register", bytes.NewReader(body)))
	return rec
}

// inventoryCount is how many relays the operator inventory lists.
func inventoryCount(t *testing.T, server http.Handler) int {
	t.Helper()
	rec := getInventory(t, server)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Relays []json.RawMessage `json:"relays"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return len(envelope.Relays)
}

// A registration whose endpoint the broker cannot reach is refused with the
// reason, and nothing is stored: the relay's own retries
// re-probe it, and the first reachable attempt registers it probed. A relay
// that registered reachable and later goes dark is withheld by the heartbeat
// re-probe, which is unchanged.
func TestRegisterRefusesUnreachableEndpoint(t *testing.T) {
	prober := &stubProber{err: errors.New("connect: operation timed out")}
	// A nanosecond interval makes every heartbeat re-probe.
	server := probeServer(t, prober, time.Nanosecond)
	req := validRegisterRequest()
	req.PublicHost = "203.0.113.7"

	rec := registerRaw(t, server, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable registration: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// No Retry-After: the relay would wait it out silently between its
	// startup attempts and delay the logged refusal the installer reads.
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none", got)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	// The leading phrase and the endpoint are what the relay logs and the
	// volunteer installer greps for.
	if !strings.HasPrefix(refusal.Error, "public endpoint unreachable: the broker could not connect to 203.0.113.7:443") || !strings.Contains(refusal.Error, "inbound TCP 443") {
		t.Fatalf("refusal body = %q", refusal.Error)
	}
	if prober.count() != 1 || prober.endpoints[0] != "203.0.113.7:443" {
		t.Fatalf("probed endpoints = %v, want the advertised endpoint once", prober.endpoints)
	}
	if ids := listedIDs(t, server); len(ids) != 0 {
		t.Fatalf("refused relay listed: %v", ids)
	}
	if got := inventoryCount(t, server); got != 0 {
		t.Fatalf("refused relay stored: inventory lists %d relays", got)
	}

	// The relay retries; once the port opens it registers, probed.
	prober.set(nil)
	desc, fields := registerOn(t, server, req)
	if got := string(fields["reachable"]); got != "true" {
		t.Fatalf("registration reachable = %s, want true", got)
	}
	if prober.count() != 2 {
		t.Fatalf("probes = %d, want one per registration attempt", prober.count())
	}
	if ids := listedIDs(t, server); len(ids) != 1 || ids[0] != desc.ID {
		t.Fatalf("listed relays = %v, want [%s]", ids, desc.ID)
	}
	if got := inventoryReachability(t, server, desc.ID); got != "true" {
		t.Fatalf("inventory reachable = %s, want true", got)
	}

	// Going dark later is caught by the heartbeat re-probe and withheld,
	// without ending the registration.
	prober.set(errors.New("connect: connection refused"))
	heartbeatOn(t, server, desc)
	if ids := listedIDs(t, server); len(ids) != 0 {
		t.Fatalf("relay listed after a failed re-probe: %v", ids)
	}
	if got := inventoryReachability(t, server, desc.ID); got != "false" {
		t.Fatalf("inventory reachable after going dark = %s, want false", got)
	}
	prober.set(nil)
	heartbeatOn(t, server, desc)
	if ids := listedIDs(t, server); len(ids) != 1 {
		t.Fatalf("repaired relay not relisted: %v", ids)
	}
}

// A refused registration hands its new-identity budget unit back, so a
// relay crash-looping on a closed port cannot exhaust its own address's cap
// and lock itself out once the port opens.
func TestRefusedRegistrationReleasesTheNewRelayCap(t *testing.T) {
	prober := &stubProber{err: errors.New("connect: operation timed out")}
	const cap = 2
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), APIToken: testAPIToken, RelayProber: prober, RelayProbeInterval: time.Hour, MaxNewRelayIDsPerIPPerDay: cap})
	for i := 0; i < 3*cap; i++ {
		if rec := registerRaw(t, server, validRegisterRequest()); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: expected 422, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	prober.set(nil)
	if rec := registerRaw(t, server, validRegisterRequest()); rec.Code != http.StatusCreated {
		t.Fatalf("registration after the port opened: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHeartbeatReprobesOnlyOnceTheResultIsStale(t *testing.T) {
	prober := &stubProber{}
	server := probeServer(t, prober, time.Hour)
	desc, fields := registerOn(t, server, validRegisterRequest())
	if got := string(fields["reachable"]); got != "true" {
		t.Fatalf("registration reachable = %s, want true", got)
	}
	heartbeatOn(t, server, desc)
	heartbeatOn(t, server, desc)
	if prober.count() != 1 {
		t.Fatalf("probes = %d, want only the registration-time probe within the interval", prober.count())
	}
}

func TestProbeSkipsTunnelRelaysAndDisabledProber(t *testing.T) {
	prober := &stubProber{err: errors.New("unreachable")}
	server := probeServer(t, prober, time.Nanosecond)
	req := validRegisterRequest()
	req.Transport = relay.TransportTunnel
	req.ExitHost = "198.51.100.7"
	desc, fields := registerOn(t, server, req)
	if _, ok := fields["reachable"]; ok {
		t.Fatalf("tunnel registration reported a probe result: %s", fields["reachable"])
	}
	heartbeatOn(t, server, desc)
	if prober.count() != 0 {
		t.Fatalf("tunnel relay probed %d times", prober.count())
	}
	if ids := listedIDs(t, server); len(ids) != 1 {
		t.Fatalf("tunnel relay not listed: %v", ids)
	}
	if got := inventoryReachability(t, server, desc.ID); got != "null" {
		t.Fatalf("inventory reachable = %s, want null", got)
	}

	// No prober configured: direct relays are listed on their lease alone.
	off := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	_, fields = registerOn(t, off, validRegisterRequest())
	if _, ok := fields["reachable"]; ok {
		t.Fatalf("registration reported a probe result with probing disabled: %s", fields["reachable"])
	}
	if ids := listedIDs(t, off); len(ids) != 1 {
		t.Fatalf("relay not listed with probing disabled: %v", ids)
	}
}

func TestInconclusiveProbeLeavesTheStoredResultAlone(t *testing.T) {
	prober := &stubProber{err: ErrProbeInconclusive}
	server := probeServer(t, prober, time.Nanosecond)
	desc, fields := registerOn(t, server, validRegisterRequest())
	// Nothing was learned: the relay is unprobed, and listed.
	if _, ok := fields["reachable"]; ok {
		t.Fatalf("inconclusive registration probe reported a result: %s", fields["reachable"])
	}
	if ids := listedIDs(t, server); len(ids) != 1 {
		t.Fatalf("unprobed relay not listed: %v", ids)
	}

	prober.set(nil)
	heartbeatOn(t, server, desc)
	if got := inventoryReachability(t, server, desc.ID); got != "true" {
		t.Fatalf("inventory reachable = %s, want true", got)
	}
	// An inconclusive re-probe keeps the previous success.
	prober.set(errors.Join(ErrProbeInconclusive, errors.New("network is unreachable")))
	heartbeatOn(t, server, desc)
	if got := inventoryReachability(t, server, desc.ID); got != "true" {
		t.Fatalf("inventory reachable after inconclusive re-probe = %s, want true", got)
	}
	if ids := listedIDs(t, server); len(ids) != 1 {
		t.Fatalf("relay delisted by an inconclusive probe: %v", ids)
	}
}

func TestProbeResultIsScopedToTheRegistrationThatRequestedIt(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		first, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now), now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !first.ReachabilityCheckedAt.IsZero() || first.Reachable {
			t.Fatalf("fresh registration carries a probe result: %+v", first)
		}
		if err := store.UpdateReachability(first.ID, first.LeaseToken, false, now); err != nil {
			t.Fatal(err)
		}
		stored, err := store.RelayByID(first.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if !relayUnreachable(stored) || !stored.ReachabilityCheckedAt.Equal(now) {
			t.Fatalf("stored result = reachable %v at %s, want unreachable at %s", stored.Reachable, stored.ReachabilityCheckedAt, now)
		}

		// The same identity re-registers: a new incarnation starts unprobed,
		// and a late write for the old lease token is refused.
		second, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now.Add(time.Second)), now.Add(time.Second), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !second.ReachabilityCheckedAt.IsZero() {
			t.Fatalf("re-registration kept the previous probe result: %+v", second)
		}
		if err := store.UpdateReachability(second.ID, first.LeaseToken, true, now.Add(2*time.Second)); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("stale lease token wrote a probe result: %v", err)
		}
		if err := store.UpdateReachability("relay_missing", "", true, now); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("unknown relay: %v", err)
		}
		if err := store.UpdateReachability(second.ID, second.LeaseToken, true, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		stored, err = store.RelayByID(second.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if relayUnreachable(stored) || !stored.ReachabilityCheckedAt.Equal(now.Add(2*time.Second)) {
			t.Fatalf("stored result after re-registration = %+v", stored)
		}
	})
}

func TestTCPRelayProber(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	prober := NewTCPRelayProber(2 * time.Second)
	// The real policy refuses loopback, so the dial path is exercised with a
	// permissive one; the policy itself is tested below.
	prober.permit = func(netip.Addr) bool { return true }

	if err := prober.Probe(context.Background(), addr.IP.String(), addr.Port); err != nil {
		t.Fatalf("listening endpoint: %v", err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().(*net.TCPAddr)
	closed.Close()
	err = prober.Probe(context.Background(), closedAddr.IP.String(), closedAddr.Port)
	if err == nil || errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("closed endpoint: %v, want a conclusive failure", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := prober.Probe(cancelled, addr.IP.String(), addr.Port); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("cancelled probe: %v, want inconclusive", err)
	}
}

// TestTCPRelayProberRefusesNonPublicTargets pins the policy that closes the
// connect oracle: a registration naming a loopback, private, link-local or
// broker-local endpoint is never dialled, so its result is inconclusive
// whether or not something listens there.
func TestTCPRelayProberRefusesNonPublicTargets(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	prober := NewTCPRelayProber(2 * time.Second)

	// A loopback literal with a live listener: refused, not reachable.
	if err := prober.Probe(context.Background(), addr.IP.String(), addr.Port); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("loopback literal: %v, want inconclusive", err)
	}
	for _, host := range []string{"10.1.2.3", "192.168.1.1", "172.16.0.9", "169.254.169.254", "100.64.0.1", "0.0.0.0", "255.255.255.255", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "ff02::1"} {
		if err := prober.Probe(context.Background(), host, addr.Port); !errors.Is(err, ErrProbeInconclusive) {
			t.Fatalf("%s: %v, want inconclusive", host, err)
		}
	}

	// A hostname is judged by what it resolves to, and dialled by address.
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.1")}, nil
	}
	if err := prober.Probe(context.Background(), "relay.test", addr.Port); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("hostname resolving to non-public addresses: %v, want inconclusive", err)
	}
	// Only the permitted address of a mixed answer is dialled.
	prober.permit = func(a netip.Addr) bool { return a.IsLoopback() }
	if err := prober.Probe(context.Background(), "relay.test", addr.Port); err != nil {
		t.Fatalf("hostname with one permitted address: %v", err)
	}
	// Every permitted address is kept, in resolver order, up to the cap.
	prober.permit = func(netip.Addr) bool { return true }
	targets, err := prober.targets(context.Background(), "relay.test")
	if err != nil || len(targets) != 2 || targets[0] != netip.MustParseAddr("127.0.0.1") || targets[1] != netip.MustParseAddr("10.0.0.1") {
		t.Fatalf("targets = %v, %v; want both in resolver order", targets, err)
	}
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		var many []netip.Addr
		for i := 1; i <= maxProbeAddresses+3; i++ {
			many = append(many, netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}))
		}
		return many, nil
	}
	targets, err = prober.targets(context.Background(), "relay.test")
	if err != nil || len(targets) != maxProbeAddresses || targets[0] != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("targets = %v, %v; want the first %d", targets, err, maxProbeAddresses)
	}
	prober.permit = probeAddressPermitted
	// A resolver failure that may clear is inconclusive; an unknown name is
	// a conclusive failure, as it is for every client.
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	}
	if err := prober.Probe(context.Background(), "relay.test", addr.Port); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("temporary resolver failure: %v, want inconclusive", err)
	}
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
	}
	if err := prober.Probe(context.Background(), "relay.test", addr.Port); err == nil || errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("unknown host: %v, want a conclusive failure", err)
	}
}

// TestTCPRelayProberFallsBackAcrossAddresses pins that a name whose first
// address fails is still reachable through a later one, and that a slow
// first address does not consume the budget the working one needs: the
// addresses are dialled together, so the probe ends on the first success.
func TestTCPRelayProberFallsBackAcrossAddresses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	const timeout = 3 * time.Second
	prober := NewTCPRelayProber(timeout)
	prober.permit = func(netip.Addr) bool { return true }
	// First address refuses (IPv6 loopback has no listener on this port, or
	// the family is absent), second accepts.
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if err := prober.Probe(context.Background(), "relay.test", port); err != nil {
		t.Fatalf("working second address: %v, want reachable", err)
	}
	// First address black-holes (TEST-NET-1 never answers), second accepts:
	// reachable well within one timeout rather than after it.
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	started := time.Now()
	if err := prober.Probe(context.Background(), "relay.test", port); err != nil {
		t.Fatalf("slow first address: %v, want reachable", err)
	}
	if elapsed := time.Since(started); elapsed >= timeout {
		t.Fatalf("probe took %s, want less than the %s budget", elapsed, timeout)
	}
	// Every address failing conclusively is unreachable.
	prober.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if err := prober.Probe(context.Background(), "relay.test", closedPort); err == nil || errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("all addresses refusing: %v, want a conclusive failure", err)
	}
}

func TestProbeAddressPermitted(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.8.8.8", "10.0.0.1", "172.31.255.255", "192.168.0.1", "169.254.169.254",
		"100.127.0.1", "0.0.0.0", "0.1.2.3", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1",
		"203.0.113.7", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "fc00::1", "fd12::1", "ff02::1", "64:ff9b::7f00:1", "2001:db8::1",
		"2002:7f00:1::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
	}
	for _, raw := range refused {
		if probeAddressPermitted(netip.MustParseAddr(raw)) {
			t.Errorf("%s permitted, want refused", raw)
		}
	}
	permitted := []string{"8.8.8.8", "1.1.1.1", "51.15.0.1", "2606:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8"}
	for _, raw := range permitted {
		if !probeAddressPermitted(netip.MustParseAddr(raw)) {
			t.Errorf("%s refused, want permitted", raw)
		}
	}
	if probeAddressPermitted(netip.Addr{}) {
		t.Error("zero address permitted")
	}
	// This host's own addresses are refused whatever range they fall in.
	interfaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, interfaceAddr := range interfaceAddrs {
		ipNet, ok := interfaceAddr.(*net.IPNet)
		if !ok {
			continue
		}
		local, _ := netip.AddrFromSlice(ipNet.IP)
		if probeAddressPermitted(local) {
			t.Errorf("local address %s permitted", local)
		}
	}
}

// TestDisabledProbingIgnoresStoredFailures is the retained-store regression:
// a broker restarted with probing off must relist relays a previous run
// marked unreachable, since nothing will ever re-probe them.
func TestDisabledProbingIgnoresStoredFailures(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()
	desc := registerRelayForRanking(t, store, now, "203.0.113.7", 8, 20)
	if err := store.UpdateReachability(desc.ID, "", false, now); err != nil {
		t.Fatal(err)
	}

	withProbe := NewServer(store, Config{SigningSeed: testSigningSeed(), APIToken: testAPIToken, RelayProber: &stubProber{}})
	if ids := listedIDs(t, withProbe); len(ids) != 0 {
		t.Fatalf("relay with a stored failure listed while probing is on: %v", ids)
	}
	withoutProbe := NewServer(store, Config{SigningSeed: testSigningSeed(), APIToken: testAPIToken})
	if ids := listedIDs(t, withoutProbe); len(ids) != 1 || ids[0] != desc.ID {
		t.Fatalf("relay with a stored failure not relisted with probing off: %v", ids)
	}
	// The stale result is still visible to operators.
	if got := inventoryReachability(t, withoutProbe, desc.ID); got != "false" {
		t.Fatalf("inventory reachable = %s, want the stored false", got)
	}
}

func TestClassifyProbeError(t *testing.T) {
	opErr := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", err)}
	}
	tests := []struct {
		name         string
		err          error
		inconclusive bool
	}{
		{name: "network unreachable", err: opErr(syscall.ENETUNREACH), inconclusive: true},
		{name: "no local address for the family", err: opErr(syscall.EADDRNOTAVAIL), inconclusive: true},
		{name: "temporary resolver failure", err: &net.DNSError{Err: "server misbehaving", IsTemporary: true}, inconclusive: true},
		{name: "resolver timeout", err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}, inconclusive: true},
		{name: "connection refused", err: opErr(syscall.ECONNREFUSED)},
		{name: "connect timeout", err: opErr(syscall.ETIMEDOUT)},
		{name: "unknown host", err: &net.DNSError{Err: "no such host", IsNotFound: true}},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyProbeError(test.err)
			if errors.Is(got, ErrProbeInconclusive) != test.inconclusive {
				t.Fatalf("classifyProbeError(%v) = %v, inconclusive want %v", test.err, got, test.inconclusive)
			}
		})
	}
}

func TestRelaysPanelReportsUnreachableRelays(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	reachable := registerRelayForRanking(t, store, now, "reachable.example.com", 8, 20)
	unreachable := registerRelayForRanking(t, store, now, "unreachable.example.com", 8, 20)
	if err := store.UpdateReachability(reachable.ID, "", true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateReachability(unreachable.ID, "", false, now); err != nil {
		t.Fatal(err)
	}
	unprobed := registerRelayForRanking(t, store, now, "unprobed.example.com", 8, 20)
	descriptors, err := store.List(now, 0)
	if err != nil {
		t.Fatal(err)
	}
	panel := buildRelaysPanel(descriptors, relayTelemetryStats{}, nil, now, time.Hour)
	if panel.Totals.UnreachableRelays != 1 || panel.Totals.OnlineRelays != 3 {
		t.Fatalf("totals = %+v", panel.Totals)
	}
	want := map[string]*bool{reachable.ID: boolPointer(true), unreachable.ID: boolPointer(false), unprobed.ID: nil}
	for _, row := range panel.Relays {
		expected := want[row.RelayID]
		switch {
		case expected == nil && row.Reachable != nil:
			t.Fatalf("row %s reachable = %v, want absent", row.RelayID, *row.Reachable)
		case expected != nil && (row.Reachable == nil || *row.Reachable != *expected):
			t.Fatalf("row %s reachable = %v, want %v", row.RelayID, row.Reachable, *expected)
		}
	}
}

func boolPointer(v bool) *bool { return &v }
