// SPDX-License-Identifier: GPL-3.0-or-later

package singboxruntime

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

const crashChildEnv = "OPENRUNG_FAKEIP_CRASH_CHILD_CACHE"

var crashOutbounds = []any{
	map[string]any{"type": "block", "tag": "proxy"},
	map[string]any{"type": "block", "tag": "direct"},
}

func queryA(t *testing.T, ctx context.Context, name string) netip.Addr {
	t.Helper()
	q := new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA)
	answer, err := service.FromContext[adapter.DNSRouter](ctx).Exchange(ctx, q, adapter.DNSQueryOptions{})
	if err != nil || len(answer.Answer) != 1 {
		t.Fatalf("query %s: %v %v", name, answer, err)
	}
	addr, _ := netip.AddrFromSlice(answer.Answer[0].(*dns.A).A)
	return addr.Unmap()
}

// TestFakeIPCrashChild is the child half of TestFakeIPBlockChangeAfterCrash:
// it hands out one fake address and dies without closing sing-box, as a
// crash, a forced kill or power loss would.
func TestFakeIPCrashChild(t *testing.T) {
	cache := os.Getenv(crashChildEnv)
	if cache == "" {
		t.Skip("child process only")
	}
	instance, ctx := loadFakeIPTestBoxOnBlock(t, cache, 0, false, crashOutbounds)
	if err := instance.Start(); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("ADDR " + queryA(t, ctx, "original.example").String() + "\n")
	os.Exit(0)
}

// sing-box saves the cache's allocation position only on a clean close (and
// on a timer), so after a crash a run that reopens the cache on the same
// block starts its table over and hands out again, for another hostname, the
// address an app still holds. connectcore's beginFakeIPRun therefore moves
// such a run to the next block with an empty cache: the stale address is then
// outside the table and refused, and nothing is handed out twice.
func TestFakeIPBlockChangeAfterCrash(t *testing.T) {
	crashedCache := func() (string, netip.Addr) {
		cache := filepath.Join(t.TempDir(), "fakeip.db")
		cmd := exec.Command(os.Args[0], "-test.run", "^TestFakeIPCrashChild$")
		cmd.Env = append(os.Environ(), crashChildEnv+"="+cache)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if rest, ok := strings.CutPrefix(line, "ADDR "); ok {
				return cache, netip.MustParseAddr(rest)
			}
		}
		t.Fatalf("child handed out no address:\n%s", out)
		return "", netip.Addr{}
	}

	for _, tc := range []struct {
		name        string
		block       int
		removeCache bool
		wantReissue bool
	}{
		{"same-block-same-cache", 0, false, true},
		{"next-block-empty-cache", 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, stale := crashedCache()
			if tc.removeCache {
				if err := os.Remove(cache); err != nil {
					t.Fatal(err)
				}
			}
			instance, ctx := loadFakeIPTestBoxOnBlock(t, cache, tc.block, false, crashOutbounds)
			if err := instance.Start(); err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			next := queryA(t, ctx, "different.example")
			if reissued := next == stale; reissued != tc.wantReissue {
				t.Fatalf("stale %s, new answer %s", stale, next)
			}
			if !tc.wantReissue {
				if got := routeOutcome(t, instance, stale); got != "rejected" {
					t.Fatalf("stale %s: %s", stale, got)
				}
			}
		})
	}
}

// routeOutcome reports "rejected" when the router refuses the connection
// before any outbound, else the error the block outbound returned.
func routeOutcome(t *testing.T, instance *box.Box, addr netip.Addr) string {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	instance.Router().RouteConnectionEx(t.Context(), a, adapter.InboundContext{
		InboundType: C.TypeTun, Source: M.ParseSocksaddr("172.19.0.2:12345"), Destination: M.Socksaddr{Addr: addr, Port: 80},
	}, func(err error) { done <- err })
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "reject") {
			return "rejected"
		}
		return "routed: " + errString(err)
	case <-time.After(3 * time.Second):
		t.Fatal("route timeout")
		return ""
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
