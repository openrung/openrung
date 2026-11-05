// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/connectcore/client"
)

func TestFakeIPRunStateKeepsBlockOnlyAfterCleanClose(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	first, err := beginFakeIPRun(cache)
	if err != nil || first < 0 || first >= client.FakeIPBlockCount {
		t.Fatalf("first run: %d, %v", first, err)
	}
	if state, err := readFakeIPRunState(cache); err != nil || state.Clean || state.Block != first {
		t.Fatalf("a starting run must be recorded as not closed: %+v, %v", state, err)
	}
	// The run ended without a clean close: the next run moves on and starts
	// from an empty cache.
	if err := os.WriteFile(cache, []byte("mappings"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, _ := beginFakeIPRun(cache)
	if second != (first+1)%client.FakeIPBlockCount {
		t.Fatalf("after an unclean end got block %d, want %d", second, (first+1)%client.FakeIPBlockCount)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache kept after an unclean end: %v", err)
	}
	// A clean close keeps the block and the cache, and with them the mappings.
	if err := os.WriteFile(cache, []byte("mappings"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := endFakeIPRun(cache, second); err != nil {
		t.Fatal(err)
	}
	if third, _ := beginFakeIPRun(cache); third != second {
		t.Fatalf("after a clean close got block %d, want %d", third, second)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache lost after a clean close: %v", err)
	}
	if state, _ := readFakeIPRunState(cache); state.Clean {
		t.Fatal("a starting run must be recorded as not closed")
	}
	// An unreadable record leaves no usable history; a fresh record follows.
	if err := os.WriteFile(fakeIPRunStatePath(cache), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if block, err := beginFakeIPRun(cache); err != nil || block < 0 || block >= client.FakeIPBlockCount {
		t.Fatalf("after an unreadable record: %d, %v", block, err)
	}
	if _, err := readFakeIPRunState(cache); err != nil {
		t.Fatalf("record not rewritten: %v", err)
	}
	// Nowhere to record: the run must not use the cache.
	if _, err := beginFakeIPRun(filepath.Join(t.TempDir(), "missing", "fakeip.db")); err == nil {
		t.Fatal("began a run without recording it")
	}
}

// cleanRun is a TunnelRun that reports whether its stop was clean.
type cleanRun struct {
	done  chan error
	clean bool
}

func (r *cleanRun) Done() <-chan error       { return r.done }
func (r *cleanRun) ExitedCleanly() bool      { return r.clean }
func (r *cleanRun) Stop(time.Duration) error { return nil }

type cleanRuntime struct {
	clean   bool
	fail    func(config []byte) error
	configs [][]byte
}

func (rt *cleanRuntime) Run(_ context.Context, config []byte) (TunnelRun, error) {
	rt.configs = append(rt.configs, append([]byte(nil), config...))
	if rt.fail != nil {
		if err := rt.fail(config); err != nil {
			return nil, err
		}
	}
	return &cleanRun{done: make(chan error, 1), clean: rt.clean}, nil
}

func fakeIPTestEngine(t *testing.T, cache string, rt TunnelRuntime) *Engine {
	t.Helper()
	s := New()
	if err := s.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	s.FakeIPCachePath = cache
	s.TunnelRuntime = rt
	s.tunnelReady = func(context.Context, int) error { return nil }
	s.probeTunnel = func(context.Context, int) (int64, error) { return 1, nil }
	return s
}

func startFakeIPTestRun(t *testing.T, s *Engine) {
	t.Helper()
	relay := relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10")
	ctx, cancel := context.WithCancel(t.Context())
	res, err := s.startCandidate(&candidateResult{relay: relay, ctx: ctx, cancel: cancel}, s.candidateConfigInput(relay, 0))
	if err != nil {
		t.Fatal(err)
	}
	res.teardown()
}

func fakeIPv4Range(t *testing.T, config []byte) string {
	t.Helper()
	const key = `"inet4_range": "`
	i := strings.Index(string(config), key)
	if i < 0 {
		return ""
	}
	rest := string(config)[i+len(key):]
	return rest[:strings.Index(rest, `"`)]
}

// A run whose core closed the cache cleanly lets the next run keep its block
// and mappings; any other end moves the next run to another block, whose
// fresh table cannot hand out an address an app still holds.
func TestEngineFakeIPBlockFollowsCleanClose(t *testing.T) {
	for _, clean := range []bool{true, false} {
		cache := filepath.Join(t.TempDir(), "fakeip.db")
		rt := &cleanRuntime{clean: clean}
		s := fakeIPTestEngine(t, cache, rt)
		startFakeIPTestRun(t, s)
		startFakeIPTestRun(t, s)
		first, second := fakeIPv4Range(t, rt.configs[0]), fakeIPv4Range(t, rt.configs[1])
		if first == "" || second == "" || (first == second) != clean {
			t.Fatalf("clean=%v: ranges %q then %q", clean, first, second)
		}
	}
}

// A run that cannot start with the cache retries once with DNS through the
// tunnel instead of failing every candidate the same way.
func TestEngineFakeIPCacheStartFailureRetriesWithRealDNS(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	rt := &cleanRuntime{fail: func(config []byte) error {
		if strings.Contains(string(config), `"cache_file"`) {
			return errors.New("open cache-file: timeout")
		}
		return nil
	}}
	startFakeIPTestRun(t, fakeIPTestEngine(t, cache, rt))
	if len(rt.configs) != 2 || strings.Contains(string(rt.configs[1]), `"fakeip"`) {
		t.Fatalf("want one cache attempt and one real-DNS retry, got %d", len(rt.configs))
	}
	if !strings.Contains(string(rt.configs[1]), `"198.18.0.0/15"`) {
		t.Fatal("the retry dropped the synthetic-range refusal")
	}
	// Nowhere to record the run state: no cache attempt at all.
	rt = &cleanRuntime{}
	startFakeIPTestRun(t, fakeIPTestEngine(t, filepath.Join(t.TempDir(), "missing", "fakeip.db"), rt))
	if len(rt.configs) != 1 || strings.Contains(string(rt.configs[0]), `"cache_file"`) {
		t.Fatal("used the cache without recording the run")
	}
}
