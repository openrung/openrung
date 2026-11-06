// SPDX-License-Identifier: GPL-3.0-or-later

package singboxruntime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

// Keep the production DNS, cache and route configuration, replacing only
// the privileged TUN and external relay with router injection and loopback
// recorders. No DNS query or application data leaves the test process.
func loadFakeIPTestBox(t *testing.T, cachePath string, lan bool, outbounds []any) (*box.Box, context.Context) {
	t.Helper()
	return loadFakeIPTestBoxOnBlock(t, cachePath, 0, lan, outbounds)
}

func loadFakeIPTestBoxOnBlock(t *testing.T, cachePath string, block int, lan bool, outbounds []any) (*box.Box, context.Context) {
	t.Helper()
	input := client.SingBoxConfigInput{FakeIPCachePath: cachePath, FakeIPBlock: block, Relay: brokerapi.RelayDescriptor{
		ID: "test", PublicHost: "203.0.113.10", PublicPort: 443,
		Protocol: brokerapi.ProtocolVLESSRealityVision, Flow: brokerapi.FlowVision, ExitMode: brokerapi.ExitModeDirect,
		ClientID: "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff", RealityPublicKey: "public-key", ShortID: "5f7a8d9c01ab23cd", ServerName: "example.org",
	}}
	if lan {
		input.SplitTunnel = &client.SplitTunnelRules{BypassLAN: true}
	}
	raw, err := client.BuildSingBoxConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "inbounds")
	cfg["outbounds"] = outbounds
	cfg["log"] = map[string]any{"disabled": true}
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithDefaultRegistry(t.Context())
	instance, ctx, err := Load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	return instance, ctx
}

func TestFakeIPCacheSurvivesTunnelRuns(t *testing.T) {
	seen := make(chan string, 8)
	outbound := func(tag string) map[string]any {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- tag + " " + r.Host
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(srv.Close)
		host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		n, _ := strconv.Atoi(port)
		return map[string]any{"type": "http", "tag": tag, "server": host, "server_port": n}
	}
	outbounds := []any{outbound("proxy"), outbound("direct")}
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	start := func(path string, lan bool) (*box.Box, context.Context, func()) {
		instance, ctx := loadFakeIPTestBox(t, path, lan, outbounds)
		if err := instance.Start(); err != nil {
			t.Fatal(err)
		}
		closed := false
		closeRun := func() {
			if !closed {
				closed = true
				if err := instance.Close(); err != nil {
					t.Error(err)
				}
			}
		}
		t.Cleanup(closeRun)
		return instance, ctx, closeRun
	}
	query := func(ctx context.Context, name string, qtype uint16) netip.Addr {
		q := new(dns.Msg).SetQuestion(dns.Fqdn(name), qtype)
		answer, err := service.FromContext[adapter.DNSRouter](ctx).Exchange(ctx, q, adapter.DNSQueryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(answer.Answer) != 1 || answer.Answer[0].Header().Ttl != 1 {
			t.Fatalf("unexpected DNS response: %v", answer)
		}
		var ip net.IP
		switch rr := answer.Answer[0].(type) {
		case *dns.A:
			ip = rr.A
		case *dns.AAAA:
			ip = rr.AAAA
		default:
			t.Fatal(answer)
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			t.Fatal(ip)
		}
		return addr.Unmap()
	}
	route := func(instance *box.Box, addr netip.Addr) string {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		done := make(chan error, 1)
		instance.Router().RouteConnectionEx(t.Context(), a, adapter.InboundContext{
			InboundType: C.TypeTun, Source: M.ParseSocksaddr("172.19.0.2:12345"), Destination: M.Socksaddr{Addr: addr, Port: 80},
		}, func(err error) { done <- err })
		select {
		case got := <-seen:
			return got
		case <-done:
			select {
			case got := <-seen:
				return got
			default:
				return "rejected"
			}
		case <-time.After(3 * time.Second):
			t.Fatal("route timeout")
			return ""
		}
	}
	first, ctx, closeFirst := start(cache, false)
	ipv4 := query(ctx, "original.example", dns.TypeA)
	ipv6 := query(ctx, "original.example", dns.TypeAAAA)
	if got := route(first, ipv4); got != "proxy original.example:80" {
		t.Fatal(got)
	}
	closeFirst()

	// A new Box models a new child process or a desktop/CLI handoff. Query a
	// different domain first, so retention cannot pass by allocation order.
	second, ctx, closeSecond := start(cache, false)
	if other := query(ctx, "different.example", dns.TypeA); other == ipv4 {
		t.Fatal("reassigned the previous run's address")
	}
	for _, tc := range []struct {
		qtype uint16
		addr  netip.Addr
	}{{dns.TypeA, ipv4}, {dns.TypeAAAA, ipv6}} {
		if got := query(ctx, "original.example", tc.qtype); got != tc.addr {
			t.Fatalf("mapping changed: %s -> %s", tc.addr, got)
		}
		if got := route(second, tc.addr); got != "proxy original.example:80" {
			t.Fatal(got)
		}
	}
	if got := route(second, netip.MustParseAddr("198.19.255.254")); got != "rejected" {
		t.Fatal(got)
	}
	closeSecond()

	// Opting out or enabling LAN bypass closes the cache and refuses its
	// addresses, while a real private destination still takes the bypass.
	for _, tc := range []struct {
		path string
		lan  bool
	}{{"", false}, {cache, true}} {
		instance, ctx, closeRun := start(tc.path, tc.lan)
		if service.FromContext[adapter.CacheFile](ctx) != nil {
			t.Fatal("unexpected cache outside fake-IP mode")
		}
		for _, addr := range []netip.Addr{ipv4, ipv6} {
			if got := route(instance, addr); got != "rejected" {
				t.Fatalf("stale %s: %s", addr, got)
			}
		}
		if tc.lan {
			if got := route(instance, netip.MustParseAddr("192.168.1.10")); got != "direct 192.168.1.10:80" {
				t.Fatal(got)
			}
		}
		closeRun()
	}
}

func TestFakeIPCacheOpenFailureDoesNotStart(t *testing.T) {
	// A directory cannot be opened as a database. The runtime must propagate
	// the cache error instead of silently starting with a fresh in-memory map.
	instance, _ := loadFakeIPTestBox(t, t.TempDir(), false, []any{map[string]any{"type": "http", "tag": "proxy", "server": "127.0.0.1", "server_port": 1}, map[string]any{"type": "direct", "tag": "direct"}})
	if err := instance.Start(); err == nil {
		_ = instance.Close()
		t.Fatal("started without the configured persistent cache")
	} else if !strings.Contains(err.Error(), "cache-file") {
		t.Fatalf("startup failed for a different reason: %v", err)
	}
}

func TestFakeIPCacheHasOneActiveOwner(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	outbounds := []any{map[string]any{"type": "http", "tag": "proxy", "server": "127.0.0.1", "server_port": 1}, map[string]any{"type": "direct", "tag": "direct"}}
	first, _ := loadFakeIPTestBox(t, cache, false, outbounds)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = first.Close()
		}
	})
	second, _ := loadFakeIPTestBox(t, cache, false, outbounds)
	// sing-box waits for its database lock, then reports the startup error.
	// A second host must not silently fall back to an independent table.
	if err := second.Start(); err == nil {
		_ = second.Close()
		t.Fatal("two runtimes opened the same cache")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	third, _ := loadFakeIPTestBox(t, cache, false, outbounds)
	if err := third.Start(); err != nil {
		t.Fatalf("cache ownership not released on shutdown: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
