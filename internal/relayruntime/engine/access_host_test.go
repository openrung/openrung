// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

const (
	testAccessHost     = "2001:db8:1::10"
	testIPv4AccessHost = "198.51.100.9"
)

func TestValidateAccessHost(t *testing.T) {
	base := Config{
		BrokerURL:  "http://broker.test",
		Mode:       ModeDirect,
		PublicHost: "203.0.113.7",
		AccessHost: testAccessHost,
	}
	if err := base.withDefaults().validate(); err != nil {
		t.Fatalf("valid access host config rejected: %v", err)
	}
	for _, listenHost := range []string{"", "::", "dual", "both", "0.0.0.0"} {
		cfg := base
		cfg.ListenHost = listenHost
		if err := cfg.withDefaults().validate(); err != nil {
			t.Errorf("listen host %q rejected: %v", listenHost, err)
		}
	}
	// An IPv4 access address beside an IPv6 public host is the mirror posture.
	v4 := base
	v4.PublicHost, v4.AccessHost = "2001:db8:1::1", testIPv4AccessHost
	if err := v4.withDefaults().validate(); err != nil {
		t.Fatalf("valid IPv4 access host config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"private access host":   func(c *Config) { c.AccessHost = "fd00::10" },
		"private ipv4":          func(c *Config) { c.AccessHost = "10.0.0.9" },
		"cgnat ipv4":            func(c *Config) { c.AccessHost = "100.64.0.9" },
		"ipv4-mapped":           func(c *Config) { c.AccessHost = "::ffff:198.51.100.9" },
		"ipv4 same as public":   func(c *Config) { c.AccessHost = "203.0.113.7" },
		"same as public host":   func(c *Config) { c.PublicHost = "2001:DB8:1:0::10" },
		"tunnel mode":           func(c *Config) { c.Mode = ModeTunnel; c.HubAddr = "hub.test:9443" },
		"auto mode with a hub":  func(c *Config) { c.Mode = ModeAuto; c.HubAddr = "hub.test:9443" },
		"literal listen host":   func(c *Config) { c.ListenHost = "203.0.113.7" },
		"ipv6-only listen host": func(c *Config) { c.ListenHost = "2001:db8:1::1" },
		"observed connections":  func(c *Config) { c.ObserveConnections = true },
		"connection log":        func(c *Config) { c.ConnectionLogOutput = io.Discard },
	} {
		cfg := base
		mutate(&cfg)
		if err := cfg.withDefaults().validate(); err == nil {
			t.Errorf("%s: validate accepted the config", name)
		}
	}
}

// fakeHostAddresses is a switchable stand-in for the netlink dump.
type fakeHostAddresses struct {
	mu        sync.Mutex
	addresses []relayruntime.HostAddress
	err       error
}

func (f *fakeHostAddresses) set(addresses []relayruntime.HostAddress, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses, f.err = addresses, err
}

func (f *fakeHostAddresses) list() ([]relayruntime.HostAddress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses, f.err
}

// A registration carries access_host only while the host check passes, the
// check runs again for every registration, and the field never disturbs the
// identity proof the broker verifies.
func TestRegistrationCarriesVerifiedAccessHostOnly(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	log := &syncBuffer{}
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		AccessHost:  testAccessHost,
		Label:       "access-relay",
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{Log: log})
	eng.cfg.HeartbeatInterval = 50 * time.Millisecond
	hosts := &fakeHostAddresses{}
	// Present but still preferred: the kernel could source egress from it.
	hosts.set([]relayruntime.HostAddress{{IP: netip.MustParseAddr(testAccessHost)}}, nil)
	eng.hostAddresses = hosts.list

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "access-relay", testIdentity, "", directOnlyListenHost)
	}()
	defer func() { cancel(); <-done }()

	eventually(t, 5*time.Second, "first registration", func() bool {
		registers, _, _ := broker.stats()
		return registers == 1
	})
	_, _, first := broker.stats()
	if first.AccessHost != "" {
		t.Fatalf("unverified access host was advertised: %q", first.AccessHost)
	}
	if !strings.Contains(log.String(), "not advertising access host "+testAccessHost) ||
		!strings.Contains(log.String(), "not deprecated") {
		t.Fatalf("missing refusal log line; log:\n%s", log.String())
	}

	// The operator fixes the host; the next registration verifies again and
	// advertises the address.
	hosts.set([]relayruntime.HostAddress{
		{IP: netip.MustParseAddr("2001:db8:1::1")},
		{IP: netip.MustParseAddr(testAccessHost), Deprecated: true},
	}, nil)
	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool {
		registers, _, _ := broker.stats()
		return registers == 2
	})
	_, _, second := broker.stats()
	if second.AccessHost != testAccessHost {
		t.Fatalf("verified access host not advertised: %q", second.AccessHost)
	}
	if second.PublicHost != "203.0.113.7" {
		t.Fatalf("public host = %q, want the configured public endpoint", second.PublicHost)
	}
	if _, err := relay.VerifyIdentity(second, time.Now()); err != nil {
		t.Fatalf("identity proof with access_host does not verify: %v", err)
	}

	// A failing query withdraws it again.
	hosts.set(nil, errors.New("netlink unavailable"))
	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "third registration", func() bool {
		registers, _, _ := broker.stats()
		return registers == 3
	})
	if _, _, third := broker.stats(); third.AccessHost != "" {
		t.Fatalf("access host advertised although the host query failed: %q", third.AccessHost)
	}
}

// fakeIPv4Host is a switchable stand-in for the IPv4 interface list and the
// routing-table dump.
type fakeIPv4Host struct {
	mu        sync.Mutex
	addresses []netip.Addr
	routes    []relayruntime.IPv4Route
}

func (f *fakeIPv4Host) set(addresses []netip.Addr, routes []relayruntime.IPv4Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses, f.routes = addresses, routes
}

func (f *fakeIPv4Host) list() ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addresses, nil
}

func (f *fakeIPv4Host) routeTable() ([]relayruntime.IPv4Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routes, nil
}

// An IPv4 access address beside an IPv6 public host is advertised only while
// it is on an interface and no route, for any destination, would make it the
// source of outgoing IPv4; the IPv6 address flags are never consulted for it.
func TestRegistrationCarriesVerifiedIPv4AccessHostOnly(t *testing.T) {
	prev := accessRecheckInterval
	accessRecheckInterval = 40 * time.Millisecond
	t.Cleanup(func() { accessRecheckInterval = prev })

	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	log := &syncBuffer{}
	const publicHost = "2001:db8:1::1"
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  publicHost,
		AccessHost:  testIPv4AccessHost,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{Log: log})
	eng.cfg.HeartbeatInterval = time.Hour // only the access recheck re-registers
	primary, access := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr(testIPv4AccessHost)
	addresses := []netip.Addr{primary, access}
	route := func(dst string, src netip.Addr) relayruntime.IPv4Route {
		return relayruntime.IPv4Route{Table: 254, Dst: netip.MustParsePrefix(dst), PrefSrc: src}
	}
	isolated := []relayruntime.IPv4Route{route("0.0.0.0/0", primary), route("172.31.1.1/32", primary)}
	host := &fakeIPv4Host{}
	// The default route uses the primary, but one destination is sourced
	// from the access address.
	host.set(addresses, append(append([]relayruntime.IPv4Route(nil), isolated...), route("192.0.2.0/24", access)))
	eng.hostIPv4Addresses, eng.ipv4Routes = host.list, host.routeTable
	eng.hostAddresses = func() ([]relayruntime.HostAddress, error) {
		t.Error("IPv6 address flags queried for an IPv4 access host")
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "access-relay", testIdentity, "", directOnlyListenHost)
	}()
	defer func() { cancel(); <-done }()

	eventually(t, 5*time.Second, "first registration", func() bool {
		registers, _, _ := broker.stats()
		return registers == 1
	})
	if _, _, first := broker.stats(); first.AccessHost != "" {
		t.Fatalf("access host advertised although a route sources from it: %q", first.AccessHost)
	}
	if !strings.Contains(log.String(), "not advertising access host "+testIPv4AccessHost) ||
		!strings.Contains(log.String(), "route to 192.0.2.0/24 (table 254) uses "+testIPv4AccessHost) {
		t.Fatalf("missing refusal log line; log:\n%s", log.String())
	}

	// The operator removes that route; every route now names the primary.
	host.set(addresses, isolated)
	eventually(t, 5*time.Second, "advertised", func() bool {
		registers, _, last := broker.stats()
		return registers == 2 && last.AccessHost == testIPv4AccessHost
	})
	_, _, second := broker.stats()
	if second.PublicHost != publicHost {
		t.Fatalf("public host = %q, want the configured IPv6 endpoint", second.PublicHost)
	}
	if _, err := relay.VerifyIdentity(second, time.Now()); err != nil {
		t.Fatalf("identity proof with an IPv4 access_host does not verify: %v", err)
	}

	// A route without a preferred source withdraws it: the kernel may pick
	// the access address for that destination.
	host.set(addresses, append(append([]relayruntime.IPv4Route(nil), isolated...), route("198.18.0.0/15", netip.Addr{})))
	eventually(t, 5*time.Second, "withdrawn for a route without src", func() bool {
		registers, _, last := broker.stats()
		return registers == 3 && last.AccessHost == ""
	})

	// Restored, then the address leaves the interface.
	host.set(addresses, isolated)
	eventually(t, 5*time.Second, "restored", func() bool {
		registers, _, last := broker.stats()
		return registers == 4 && last.AccessHost == testIPv4AccessHost
	})
	host.set([]netip.Addr{primary}, isolated)
	eventually(t, 5*time.Second, "withdrawn when absent", func() bool {
		registers, _, last := broker.stats()
		return registers == 5 && last.AccessHost == ""
	})
}

// Without an access host configured the host is never queried and nothing
// new is sent.
func TestRegistrationWithoutAccessHostSkipsTheHostQuery(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	queried := false
	eng.hostAddresses = func() ([]relayruntime.HostAddress, error) { queried = true; return nil, nil }
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	if _, _, last := broker.stats(); last.AccessHost != "" {
		t.Fatalf("access host sent without one configured: %q", last.AccessHost)
	}
	eng.Stop()
	if queried {
		t.Fatal("the host addresses were queried without an access host configured")
	}
}

// An access-address relay renders exactly one public inbound on the dual-stack
// wildcard, for the plain and the WSS (LISTEN_HOST=0.0.0.0) postures alike:
// the access address is served by the same socket as the public endpoint,
// never by a second listener.
func TestRenderXrayConfigAccessHostKeepsOneWildcardListener(t *testing.T) {
	for name, cfg := range map[string]Config{
		"direct default listen host": {
			BrokerURL: "https://broker.test", FoundationToken: "fnd-secret", Mode: ModeDirect,
			PublicHost: "203.0.113.7", AccessHost: testAccessHost, Identity: testIdentity,
		},
		"wss with 0.0.0.0": {
			BrokerURL: "https://broker.test", FoundationToken: "fnd-secret", Mode: ModeDirect,
			PublicHost: "203.0.113.7", AccessHost: testAccessHost, ListenHost: "0.0.0.0",
			WSSFronts: wssTestFronts(), Identity: testIdentity,
		},
	} {
		t.Run(name, func(t *testing.T) {
			parsed := renderAndParse(t, New(cfg, Events{}))
			if parsed.Inbounds[0].Listen != "0.0.0.0" || parsed.Inbounds[0].Port != 443 {
				t.Fatalf("inbound = %s:%d, want the 0.0.0.0:443 dual-stack wildcard", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port)
			}
		})
	}
}

// With no configured public host, detection skips the access address (which
// sits on the same interface as the primary), and a resolved public host that
// still names the access address is never advertised beside it.
func TestDetectedPublicHostNeverIsTheAccessHost(t *testing.T) {
	const primary = "2001:db8:1::1"
	for name, tc := range map[string]struct {
		detect     func(exclude string) (string, error)
		wantPublic string
		wantAccess string
	}{
		"detection skips the access host": {
			detect: func(exclude string) (string, error) {
				// The access address enumerates first, as it can on a host.
				for _, candidate := range []string{testAccessHost, primary} {
					if candidate != exclude {
						return candidate, nil
					}
				}
				return "", errors.New("no global IPv6")
			},
			wantPublic: primary,
			wantAccess: testAccessHost,
		},
		"a resolved public host equal to the access host": {
			detect:     func(string) (string, error) { return testAccessHost, nil },
			wantPublic: testAccessHost,
			wantAccess: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			prev := detectPublicIPv6
			var excluded []string
			var mu sync.Mutex
			detectPublicIPv6 = func(exclude string) (string, error) {
				mu.Lock()
				excluded = append(excluded, exclude)
				mu.Unlock()
				return tc.detect(exclude)
			}
			t.Cleanup(func() { detectPublicIPv6 = prev })

			broker := &fakeBroker{}
			ts := httptest.NewServer(broker.handler())
			defer ts.Close()
			log := &syncBuffer{}
			eng := New(Config{
				BrokerURL:   ts.URL,
				Mode:        ModeDirect,
				AccessHost:  testAccessHost,
				ListenPort:  freePort(t),
				Identity:    testIdentity,
				DisableXray: true,
				ConfigDir:   t.TempDir(),
			}, Events{Log: log})
			eng.hostAddresses = func() ([]relayruntime.HostAddress, error) {
				return []relayruntime.HostAddress{{IP: netip.MustParseAddr(testAccessHost), Deprecated: true}}, nil
			}
			if err := eng.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer eng.Stop()
			eventually(t, 5*time.Second, "registration", func() bool {
				registers, _, _ := broker.stats()
				return registers >= 1
			})
			_, _, last := broker.stats()
			if last.PublicHost != tc.wantPublic || last.AccessHost != tc.wantAccess {
				t.Fatalf("registered public %q access %q; want public %q access %q", last.PublicHost, last.AccessHost, tc.wantPublic, tc.wantAccess)
			}
			if tc.wantAccess == "" && !strings.Contains(log.String(), "it is the public host") {
				t.Fatalf("missing refusal log line; log:\n%s", log.String())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, exclude := range excluded {
				if exclude != testAccessHost {
					t.Fatalf("detection ran without excluding the access host: %q", excluded)
				}
			}
		})
	}
}

// An access address that disappears after registration is withdrawn from the
// broker within one recheck interval, and advertised again once it is back —
// without restarting the session.
func TestAccessHostIsWithdrawnAndRestoredWithoutRestart(t *testing.T) {
	prev := accessRecheckInterval
	accessRecheckInterval = 40 * time.Millisecond
	t.Cleanup(func() { accessRecheckInterval = prev })

	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	log := &syncBuffer{}
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		AccessHost:  testAccessHost,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{Log: log})
	eng.cfg.HeartbeatInterval = time.Hour // only the access recheck re-registers
	present := []relayruntime.HostAddress{{IP: netip.MustParseAddr(testAccessHost), Deprecated: true}}
	hosts := &fakeHostAddresses{}
	hosts.set(present, nil)
	eng.hostAddresses = hosts.list

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "access-relay", testIdentity, "", directOnlyListenHost)
	}()
	defer func() { cancel(); <-done }()

	eventually(t, 5*time.Second, "first registration", func() bool {
		registers, _, last := broker.stats()
		return registers == 1 && last.AccessHost == testAccessHost
	})
	// Unchanged state never re-registers.
	time.Sleep(5 * accessRecheckInterval)
	if registers, _, _ := broker.stats(); registers != 1 {
		t.Fatalf("re-registered %d times with nothing changed", registers-1)
	}

	hosts.set(nil, nil) // the network manager dropped the address
	eventually(t, 5*time.Second, "withdrawal", func() bool {
		registers, _, last := broker.stats()
		return registers == 2 && last.AccessHost == ""
	})
	if !strings.Contains(log.String(), "no longer usable") {
		t.Fatalf("missing the withdrawal log line; log:\n%s", log.String())
	}
	time.Sleep(5 * accessRecheckInterval)
	if registers, _, _ := broker.stats(); registers != 2 {
		t.Fatalf("kept re-registering while the address stayed missing (%d)", registers)
	}

	hosts.set(present, nil) // restored
	eventually(t, 5*time.Second, "restoration", func() bool {
		registers, _, last := broker.stats()
		return registers == 3 && last.AccessHost == testAccessHost
	})
	if _, _, last := broker.stats(); last.PublicHost != "203.0.113.7" {
		t.Fatalf("public host changed: %q", last.PublicHost)
	}
	select {
	case err := <-done:
		t.Fatalf("the session ended: %v", err)
	default:
	}
}

// A relay without an access host never arms the recheck.
func TestNoAccessHostNeverRechecks(t *testing.T) {
	prev := accessRecheckInterval
	accessRecheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { accessRecheckInterval = prev })
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL: ts.URL, Mode: ModeDirect, PublicHost: "203.0.113.7",
		ListenPort: freePort(t), Identity: testIdentity, DisableXray: true, ConfigDir: t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = time.Hour
	queried := 0
	var mu sync.Mutex
	eng.hostAddresses = func() ([]relayruntime.HostAddress, error) { mu.Lock(); queried++; mu.Unlock(); return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "plain", testIdentity, "", directOnlyListenHost)
	}()
	eventually(t, 5*time.Second, "registration", func() bool { r, _, _ := broker.stats(); return r == 1 })
	time.Sleep(10 * accessRecheckInterval)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if queried != 0 {
		t.Fatalf("host addresses queried %d times without an access host", queried)
	}
	if r, _, _ := broker.stats(); r != 1 {
		t.Fatalf("re-registered without an access host: %d", r)
	}
}

// An access-host re-registration mints a new lease token while the lease
// syncer may be holding a long poll on the old one. The stale poll is
// cancelled and the new session is synchronised and acknowledged at once —
// not after a heartbeat — so leases issued meanwhile are applied within the
// broker's acknowledgement wait.
func TestAccessHostReRegistrationResyncsLeasesImmediately(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	prev := accessRecheckInterval
	accessRecheckInterval = 40 * time.Millisecond
	t.Cleanup(func() { accessRecheckInterval = prev })

	broker := &fakeBroker{leaseCapable: true, rejectStaleLeaseTokens: true, leaseHold: 20 * time.Second}
	users := newRecordingUsers()
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		AccessHost:  testAccessHost,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = time.Hour // a 404 wait would outlast the test
	eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
	hosts := &fakeHostAddresses{}
	hosts.set([]relayruntime.HostAddress{{IP: netip.MustParseAddr(testAccessHost), Deprecated: true}}, nil)
	eng.hostAddresses = hosts.list

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "access-relay", testIdentity, "", directOnlyListenHost)
	}()
	defer func() { cancel(); <-done }()

	eventually(t, 5*time.Second, "online with the first session acked", func() bool {
		return eng.Status().Phase == PhaseOnline && len(broker.ackedRevisions()) >= 1
	})
	// Let the syncer park in a long poll on the first token.
	time.Sleep(200 * time.Millisecond)

	hosts.set(nil, nil) // the address disappears: re-register without it
	eventually(t, 5*time.Second, "withdrawal re-registration", func() bool {
		registers, _, last := broker.stats()
		return registers == 2 && last.AccessHost == ""
	})
	newToken := func() string { broker.mu.Lock(); defer broker.mu.Unlock(); return broker.currentLeaseToken() }()
	revision := broker.setLeases(relay.LeaseEntry{ID: "0123456789abcdef0123456789abcdef", Credential: "44444444-2222-4333-8444-555555555555", ExpiresAt: time.Now().Add(time.Hour)})
	eventually(t, 3*time.Second, "the new session's lease set acked with its own token", func() bool {
		last, ok := broker.lastAcked()
		if !ok || last < revision {
			return false
		}
		_, bearers := broker.leaseStats()
		return len(bearers) > 0 && bearers[len(bearers)-1] == newToken
	})
	if emails := users.leaseEmails(); len(emails) != 1 {
		t.Fatalf("installed lease users = %v, want the issued lease", emails)
	}
}

// A re-registration that lands while the lease syncer is backing off after
// a failed poll ends the backoff: the new session is synchronised at once,
// not after the (here 30-second) backoff.
func TestLeaseBackoffEndsOnReRegistration(t *testing.T) {
	shortRotation(t, time.Hour)
	minBackoff, maxBackoff, expiry := leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval
	leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = 30*time.Second, 30*time.Second, time.Hour
	t.Cleanup(func() { leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = minBackoff, maxBackoff, expiry })
	prev := accessRecheckInterval
	accessRecheckInterval = 40 * time.Millisecond
	t.Cleanup(func() { accessRecheckInterval = prev })

	broker := &fakeBroker{leaseCapable: true, rejectStaleLeaseTokens: true}
	users := newRecordingUsers()
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		AccessHost:  testAccessHost,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = time.Hour
	eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
	hosts := &fakeHostAddresses{}
	hosts.set([]relayruntime.HostAddress{{IP: netip.MustParseAddr(testAccessHost), Deprecated: true}}, nil)
	eng.hostAddresses = hosts.list

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "access-relay", testIdentity, "", directOnlyListenHost)
	}()
	defer func() { cancel(); <-done }()
	eventually(t, 5*time.Second, "online with the first session acked", func() bool {
		return eng.Status().Phase == PhaseOnline && len(broker.ackedRevisions()) >= 1
	})

	// Polls start failing; the syncer falls into its 30-second backoff.
	broker.mu.Lock()
	broker.failLeasePolls = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "a failed poll", func() bool {
		polls, _ := broker.leaseStats()
		return polls >= 2
	})
	time.Sleep(300 * time.Millisecond) // well inside the backoff
	broker.mu.Lock()
	broker.failLeasePolls = false
	broker.mu.Unlock()

	hosts.set(nil, nil) // re-register without the access host
	eventually(t, 5*time.Second, "withdrawal re-registration", func() bool {
		registers, _, last := broker.stats()
		return registers == 2 && last.AccessHost == ""
	})
	newToken := func() string { broker.mu.Lock(); defer broker.mu.Unlock(); return broker.currentLeaseToken() }()
	revision := broker.setLeases(relay.LeaseEntry{ID: "0123456789abcdef0123456789abcdef", Credential: "44444444-2222-4333-8444-555555555555", ExpiresAt: time.Now().Add(time.Hour)})
	eventually(t, 3*time.Second, "the new session acked despite the backoff", func() bool {
		last, ok := broker.lastAcked()
		if !ok || last < revision {
			return false
		}
		_, bearers := broker.leaseStats()
		return len(bearers) > 0 && bearers[len(bearers)-1] == newToken
	})
}
