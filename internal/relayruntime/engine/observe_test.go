// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"openrung/internal/relay"
)

// readWrittenXrayConfig parses the xray config a session wrote to path.
func readWrittenXrayConfig(t *testing.T, path string) renderedXrayConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written xray config: %v", err)
	}
	var parsed renderedXrayConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("written xray config is not valid JSON: %v", err)
	}
	var vless []struct {
		Listen   string `json:"listen"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	}
	for _, inbound := range parsed.Inbounds {
		if inbound.Protocol == "vless" {
			vless = append(vless, inbound)
		}
	}
	if len(vless) != 1 {
		t.Fatalf("written config has %d vless inbounds, want 1: %+v", len(vless), parsed.Inbounds)
	}
	parsed.Inbounds = vless
	return parsed
}

// runObserveSession runs a direct session with xray disabled and returns the
// engine once it is online, plus the path of the xray config it wrote.
func runObserveSession(t *testing.T, cfg Config, listenHost string) (*Engine, string) {
	t.Helper()
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	t.Cleanup(ts.Close)

	configPath := filepath.Join(t.TempDir(), "xray.json")
	cfg.BrokerURL = ts.URL
	cfg.Mode = ModeDirect
	cfg.Identity = testIdentity
	cfg.DisableXray = true
	cfg.ConfigDir = t.TempDir()
	cfg.ConfigPath = configPath
	if cfg.ListenPort == 0 {
		cfg.ListenPort = freePort(t)
	}
	eng := New(cfg, Events{})
	brokerClient := eng.cfg.brokerClient()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, brokerClient, eng.cfg, "observe-relay", testIdentity, "127.0.0.1", listenHost)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	eventually(t, 5*time.Second, "session online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	return eng, configPath
}

// An unobserved direct session (the server-relay default) lets xray own the
// public port: no engine listener sits there, and the per-connection counters
// never move.
func TestUnobservedDirectSessionLetsXrayOwnThePublicPort(t *testing.T) {
	eng, configPath := runObserveSession(t, Config{}, "127.0.0.1")
	listenPort := eng.cfg.ListenPort

	parsed := readWrittenXrayConfig(t, configPath)
	if parsed.Inbounds[0].Listen != "127.0.0.1" || parsed.Inbounds[0].Port != listenPort {
		t.Fatalf("xray inbound = %s:%d, want the public listener 127.0.0.1:%d", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port, listenPort)
	}
	// xray is disabled, so nothing may be listening: the engine itself must
	// not have opened the public port.
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the engine listens on the public port without observing")
	}
	if status := eng.Status(); status.ActiveConnections != 0 || status.TotalConnections != 0 || status.BytesFromClients != 0 || status.BytesToClients != 0 {
		t.Fatalf("unobserved counters moved: %+v", status)
	}
}

// An observed direct session (the volunteer-app posture) puts the observer on
// the public port, rebinds xray to loopback behind it, and counts traffic.
func TestObservedDirectSessionCountsConnections(t *testing.T) {
	eng, configPath := runObserveSession(t, Config{ObserveConnections: true}, "127.0.0.1")
	listenPort := eng.cfg.ListenPort

	parsed := readWrittenXrayConfig(t, configPath)
	if listen := parsed.Inbounds[0].Listen; listen != "::1" && listen != "127.0.0.1" {
		t.Fatalf("xray inbound host = %q, want a loopback host behind the observer", listen)
	}
	if parsed.Inbounds[0].Port == listenPort {
		t.Fatalf("xray inbound port = %d, want a reserved loopback port, not the public one", listenPort)
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)), time.Second)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer conn.Close()
	eventually(t, 5*time.Second, "connection counted", func() bool {
		return eng.Status().TotalConnections >= 1
	})
}

// The dual-family aliases (and the automatic probe's empty wildcard) reach
// xray as its one dual-stack wildcard when nothing observes the port.
func TestUnobservedDualListenHostAliasesBindOneWildcard(t *testing.T) {
	for _, host := range []string{"", "::", "dual", "both", " Dual "} {
		t.Run(strconv.Quote(host), func(t *testing.T) {
			eng, configPath := runObserveSession(t, Config{}, host)
			parsed := readWrittenXrayConfig(t, configPath)
			if parsed.Inbounds[0].Listen != "0.0.0.0" || parsed.Inbounds[0].Port != eng.cfg.ListenPort {
				t.Fatalf("xray inbound = %s:%d, want 0.0.0.0:%d", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port, eng.cfg.ListenPort)
			}
		})
	}
}

func TestXrayPublicListenHost(t *testing.T) {
	for host, want := range map[string]string{
		"":             "0.0.0.0",
		"::":           "0.0.0.0",
		"dual":         "0.0.0.0",
		"BOTH":         "0.0.0.0",
		"0.0.0.0":      "0.0.0.0",
		"127.0.0.1":    "127.0.0.1",
		"::1":          "::1",
		" 2001:db8::1": "2001:db8::1",
	} {
		if got := xrayPublicListenHost(host); got != want {
			t.Errorf("xrayPublicListenHost(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestConnectionLogImpliesObserving(t *testing.T) {
	if (Config{}).observeConnections() {
		t.Fatal("a zero config observes connections")
	}
	if !(Config{ObserveConnections: true}).observeConnections() {
		t.Fatal("ObserveConnections does not observe")
	}
	if !(Config{ConnectionLogOutput: io.Discard}).observeConnections() {
		t.Fatal("a connection log does not imply observing")
	}
}

// Tunnel mode never had an observer: the hub tunnel client forwards straight
// to xray on loopback whether or not direct connections are observed, and
// tunnel streams keep counting.
func TestTunnelSessionReachesXrayWithoutObserving(t *testing.T) {
	_, hubAddr := startTestHub(t)
	configPath := filepath.Join(t.TempDir(), "xray.json")
	eng := New(Config{
		Mode:         ModeTunnel,
		HubAddr:      hubAddr,
		HubPlaintext: true,
		Label:        "tunnel-observe-test",
		ListenPort:   8443,
		Identity:     testIdentity,
		DisableXray:  true,
		ConfigDir:    t.TempDir(),
		ConfigPath:   configPath,
	}, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()
	eventually(t, 5*time.Second, "tunnel online", func() bool {
		status := eng.Status()
		return status.Phase == PhaseOnline && status.Transport == relay.TransportTunnel
	})

	// Stand in for xray on the loopback address the session configured it on.
	parsed := readWrittenXrayConfig(t, configPath)
	target := net.JoinHostPort(parsed.Inbounds[0].Listen, strconv.Itoa(parsed.Inbounds[0].Port))
	if ip := net.ParseIP(parsed.Inbounds[0].Listen); ip == nil || !ip.IsLoopback() {
		t.Fatalf("tunnel xray inbound = %s, want a loopback address", target)
	}
	backend, err := net.Listen("tcp", target)
	if err != nil {
		t.Fatalf("listen as xray on %s: %v", target, err)
	}
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	client, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(eng.Status().PublicPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial hub public port: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(client, reply); err != nil || string(reply) != "ping" {
		t.Fatalf("echo through tunnel = %q, %v; want ping", reply, err)
	}
	eventually(t, 5*time.Second, "tunnel stream counted", func() bool {
		return eng.Status().TotalConnections >= 1
	})
}
