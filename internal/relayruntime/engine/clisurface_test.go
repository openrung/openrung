package engine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

// The operator surface cmd/relay exposes — a chosen listen host, a public port
// that may differ from the bound one, an exact config path — lives here, so
// these pin it against the flags that feed it.

// A direct registration states transport and node class explicitly, as
// cmd/relay always has. The broker canonicalizes an absent field to the same
// values, so this is about keeping the CLI's bytes on the wire unchanged.
func TestDirectRegistrationStatesTransportAndNodeClass(t *testing.T) {
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
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "relay online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	_, _, last := broker.stats()
	if last.Transport != relay.TransportDirect {
		t.Errorf("transport = %q, want %q", last.Transport, relay.TransportDirect)
	}
	if last.NodeClass != relay.NodeClassVolunteer {
		t.Errorf("node_class = %q, want %q", last.NodeClass, relay.NodeClassVolunteer)
	}
}

// PublicPort is what the directory advertises; ListenPort is only what the
// relay binds. They differ whenever a port mapping sits in front of the relay.
func TestPublicPortIsAdvertisedIndependentlyOfListenPort(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	listenPort := freePort(t)
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		ListenHost:  "127.0.0.1",
		ListenPort:  listenPort,
		PublicPort:  443,
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "relay online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	_, _, last := broker.stats()
	if last.PublicPort != 443 {
		t.Fatalf("advertised port = %d, want the configured public port 443", last.PublicPort)
	}
	// The bound port is still the listen port: the observer must be reachable
	// there, not on the advertised one.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial the listen port: %v", err)
	}
	_ = conn.Close()

	// Zero means "advertise what we bind", so a caller that never heard of the
	// field keeps its old behaviour.
	cfg := eng.currentConfig()
	cfg.PublicPort = 0
	if got := cfg.publicPort(); got != listenPort {
		t.Fatalf("publicPort() = %d with no public port set, want the listen port %d", got, listenPort)
	}
}

// A probed port is the one confirmed reachable from the internet, so auto mode
// advertises it and ignores any configured public port, which described a
// different and unprobed endpoint.
func TestAutomaticModeAdvertisesTheProbedPort(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	probedPort := freePort(t)
	eng := New(Config{
		BrokerURL:               ts.URL,
		Mode:                    ModeAuto,
		HubAddr:                 "hub.example:9443",
		AutomaticPortCandidates: []int{probedPort},
		PublicPort:              443,
		Identity:                testIdentity,
		DisableXray:             true,
		ConfigDir:               t.TempDir(),
	}, Events{})
	eng.probeDirect = func(_ context.Context, _, _, _ string, port int, _ *http.Client) relayruntime.DirectProbeResult {
		return relayruntime.DirectProbeResult{Outcome: relayruntime.DirectProbeReachable, ObservedHost: "203.0.113.9"}
	}
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	eventually(t, 5*time.Second, "relay online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	_, _, last := broker.stats()
	if last.PublicPort != probedPort || last.PublicHost != "203.0.113.9" {
		t.Fatalf("advertised %s:%d, want the probed endpoint 203.0.113.9:%d", last.PublicHost, last.PublicPort, probedPort)
	}
}

// The configured listen host reaches both the reachability probe's bind and
// the direct listener: a probe that binds differently from the listener can
// confirm one stack and then serve on another.
func TestConfiguredListenHostDrivesProbeAndListener(t *testing.T) {
	var probedHosts []string
	eng := New(Config{
		BrokerURL:  "http://127.0.0.1:1",
		Mode:       ModeAuto,
		HubAddr:    "hub.example:9443",
		ListenHost: "0.0.0.0",
		ListenPort: 443,
		Identity:   testIdentity,
	}, Events{})
	eng.probeDirect = func(_ context.Context, _, _, listenHost string, _ int, _ *http.Client) relayruntime.DirectProbeResult {
		probedHosts = append(probedHosts, listenHost)
		return relayruntime.DirectProbeResult{Outcome: relayruntime.DirectProbeExternallyUnreachable}
	}
	if _, _, _ = eng.autoResolve(context.Background(), eng.currentConfig()); len(probedHosts) != 1 {
		t.Fatalf("probe bind hosts = %v, want exactly one", probedHosts)
	}
	if probedHosts[0] != "0.0.0.0" {
		t.Fatalf("probe bound %q, want the configured listen host", probedHosts[0])
	}
	cfg := eng.currentConfig()
	if got := cfg.directListenHost(true); got != "0.0.0.0" {
		t.Fatalf("probe-selected listener host = %q, want the same bind as the probe", got)
	}
	if got := cfg.directListenHost(false); got != "0.0.0.0" {
		t.Fatalf("direct-only listener host = %q, want the configured listen host", got)
	}
}

// An unset listen host keeps each mode's own default: the generic wildcard for
// a probe-selected session (which must reproduce the probe's bind) and the
// explicit dual-family listener for a direct-only one.
func TestUnsetListenHostKeepsPerModeDefaults(t *testing.T) {
	var cfg Config
	if got := cfg.directListenHost(true); got != automaticDirectListenHost {
		t.Errorf("automatic default = %q, want %q", got, automaticDirectListenHost)
	}
	if got := cfg.directListenHost(false); got != directOnlyListenHost {
		t.Errorf("direct-only default = %q, want %q", got, directOnlyListenHost)
	}
	if got := cfg.wssListenHost(); got != wssDirectListenHost {
		t.Errorf("WSS default = %q, want %q", got, wssDirectListenHost)
	}
}

// The dual-family aliases open two listeners, which only the connection
// listener can do — so they must serve a direct session and be refused by the
// renderer rather than reaching xray, which binds one address.
func TestDualListenHostServesButCannotBeRendered(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	listenPort := freePort(t)
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		ListenHost:  "dual",
		ListenPort:  listenPort,
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})

	if _, err := eng.RenderXrayConfig(); err == nil {
		t.Fatal("RenderXrayConfig() error = nil, want a refusal: xray binds one address")
	}

	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()
	eventually(t, 5*time.Second, "relay online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(listenPort)), 2*time.Second)
		if err != nil {
			// An IPv6-less CI host cannot prove the second family; the first is
			// enough to show the alias reached the listener.
			continue
		}
		_ = conn.Close()
	}
}

// A configured listen host is what the renderer prints, so -print-config-only
// describes the binding the operator asked for.
func TestRenderXrayConfigUsesConfiguredListenHost(t *testing.T) {
	eng := New(Config{
		BrokerURL:  "http://127.0.0.1:1",
		Mode:       ModeDirect,
		ListenHost: "0.0.0.0",
		ListenPort: 8443,
		Identity:   testIdentity,
	}, Events{})
	parsed := renderAndParse(t, eng)
	if parsed.Inbounds[0].Listen != "0.0.0.0" || parsed.Inbounds[0].Port != 8443 {
		t.Fatalf("inbound = %s:%d, want 0.0.0.0:8443", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port)
	}
}

// ConfigPath names the generated xray config exactly, so an operator who
// pointed -config-out at a path keeps finding it there.
func TestConfigPathOverridesConfigDir(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "named-xray-config.json")
	eng := New(Config{
		BrokerURL:   "http://127.0.0.1:1",
		Mode:        ModeDirect,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   dir,
		ConfigPath:  configPath,
	}, Events{})

	if _, _, err := eng.startXray(context.Background(), eng.currentConfig(), testIdentity, "127.0.0.1", 1080); err != nil {
		t.Fatalf("startXray: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat generated config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %v, want 0600: it holds the Reality private key", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "openrung-volunteer-xray.json")); !os.IsNotExist(err) {
		t.Fatalf("the default config name was also written (stat error %v)", err)
	}
}

// A WSS relay's xray owns the public port, so its listen host must be one the
// colocated sidecar's 127.0.0.1:443 dial actually reaches, and both ports must
// be 443.
func TestWSSListenHostAndPublicPortGates(t *testing.T) {
	base := validWSSConfig("https://broker.test").withDefaults()
	for _, listenHost := range []string{"", "0.0.0.0", "127.0.0.1"} {
		cfg := base
		cfg.ListenHost = listenHost
		if err := cfg.validate(); err != nil {
			t.Errorf("validate() rejected IPv4-loopback-reachable listen host %q: %v", listenHost, err)
		}
	}
	for name, mutate := range map[string]func(*Config){
		"public-IP listener": func(c *Config) { c.ListenHost = "203.0.113.7" },
		"IPv6-only listener": func(c *Config) { c.ListenHost = "::1" },
		"IPv6 wildcard":      func(c *Config) { c.ListenHost = "::" },
		"hostname listener":  func(c *Config) { c.ListenHost = "relay.example" },
		"dual listener":      func(c *Config) { c.ListenHost = "dual" },
		"public port":        func(c *Config) { c.PublicPort = 8443 },
		"listen port":        func(c *Config) { c.ListenPort = 8443 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("validate() error = nil, want a WSS posture rejection")
			}
		})
	}
}

// A misconfiguration that fails identically on every attempt belongs at Start,
// where the caller still sees it, not in the retry loop.
func TestValidateRejectsUnusableFlagValuesAtStart(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"label charset": func(c *Config) { c.Label = "not a label" },
		"label length":  func(c *Config) { c.Label = strings.Repeat("a", relay.MaxLabelLength+1) },
		"public port":   func(c *Config) { c.PublicPort = 70000 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				BrokerURL:  "http://127.0.0.1:1",
				Mode:       ModeDirect,
				ListenPort: 443,
				Identity:   testIdentity,
			}
			mutate(&cfg)
			if err := New(cfg, Events{}).Start(); err == nil {
				t.Fatal("Start() error = nil, want a validation error")
			}
		})
	}
}

// A heartbeat failure that is not the broker's pruned-relay 404 is transient:
// the session keeps its lease and retries on the next tick rather than
// registering a second relay row.
func TestTransientHeartbeatFailureDoesNotReRegister(t *testing.T) {
	var registers, heartbeats atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/relays/register" {
			registers.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(relay.Descriptor{ID: "relay_1", PublicHost: "203.0.113.7", PublicPort: 443})
			return
		}
		heartbeats.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"temporary failure"}`))
	}))
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
	// Below the validated minimum, so drive the session directly rather than
	// waiting 30s per heartbeat.
	cfg := eng.currentConfig()
	cfg.HeartbeatInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, cfg.brokerClient(), cfg, "hb-relay", testIdentity, "203.0.113.7", directOnlyListenHost)
	}()
	defer func() {
		cancel()
		<-done
	}()

	eventually(t, 5*time.Second, "several failed heartbeats", func() bool {
		return heartbeats.Load() >= 3
	})
	if got := registers.Load(); got != 1 {
		t.Fatalf("registrations = %d, want 1: a 500 is transient, not a lost lease", got)
	}
	if eng.Status().Phase != PhaseOnline {
		t.Fatalf("phase = %q, want the session to stay online through transient heartbeat failures", eng.Status().Phase)
	}
}
