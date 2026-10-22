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

const testAccessHost = "2001:db8:1::10"

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
	for name, mutate := range map[string]func(*Config){
		"ipv4 access host":      func(c *Config) { c.AccessHost = "198.51.100.9" },
		"private access host":   func(c *Config) { c.AccessHost = "fd00::10" },
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
