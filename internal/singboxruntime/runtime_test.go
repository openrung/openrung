package singboxruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

// goldenDir is the sing-box config builder's golden corpus: every config
// shape the engine can hand the bundled runtime. Loading each one here proves
// the pinned sing-box library version accepts the builder's output — the
// same contract the "sing-box 1.14 or newer" doc line used to push onto the
// user's installed binary.
const goldenDir = "../../connectcore/client/testdata/singbox"

func TestLoadAcceptsEveryGoldenConfig(t *testing.T) {
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	var checked int
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".golden.json") {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			path := filepath.Join(goldenDir, entry.Name())
			// The mobile shapes only construct on the device: they reference
			// rule-set files at on-device absolute paths and declare the
			// clash_api service (a with_clash_api feature of the mobile
			// hosts). Parsing still proves the pinned library accepts the
			// generated shape; the terminal client never runs these configs.
			if strings.HasPrefix(entry.Name(), "mobile-") {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden: %v", err)
				}
				ctx := include.Context(context.Background())
				if _, err := json.UnmarshalExtendedContext[option.Options](ctx, content); err != nil {
					t.Fatalf("bundled sing-box rejects this generated config shape: %v", err)
				}
				return
			}
			instance, _, err := Load(context.Background(), withValidRealityKey(t, path))
			if !UTLSEnabled {
				// Every golden carries a Reality or uTLS outbound; without
				// the with_utls build tag creation must fail with upstream's
				// rebuild hint, which is the runtime's whole error story for
				// an untagged build.
				if err == nil {
					_ = instance.Close()
					t.Fatal("expected a with_utls build error, got success (did this config lose its Reality outbound?)")
				}
				if !strings.Contains(err.Error(), "with_utls") {
					t.Fatalf("expected the upstream rebuild-with-with_utls hint, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bundled sing-box rejects this generated config: %v", err)
			}
			_ = instance.Close()
		})
	}
	if checked == 0 {
		t.Fatalf("no *.golden.json under %s; did the golden corpus move?", goldenDir)
	}
}

// withValidRealityKey copies a golden config with its placeholder Reality
// public_key ("public-key") replaced by a well-formed 32-byte key: a with_utls
// build validates the key eagerly at creation, and the placeholder would fail
// on its format rather than exercising the config shape under test.
func withValidRealityKey(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	encoded := base64.RawURLEncoding.EncodeToString(key)
	replaced := bytes.ReplaceAll(content,
		[]byte(`"public_key": "public-key"`),
		[]byte(`"public_key": "`+encoded+`"`))
	out := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(out, replaced, 0o600); err != nil {
		t.Fatalf("write patched golden: %v", err)
	}
	return out
}

// TestRunServesAndStopsOnCancel exercises the full child-process body the
// client's run subcommand executes: start a loopback mixed inbound, verify it
// accepts, cancel, and require the instance to shut down cleanly.
func TestRunServesAndStopsOnCancel(t *testing.T) {
	// Reserve a loopback port. The close-reuse gap is narrow enough for tests.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	config := fmt.Sprintf(`{
  "log": {"level": "warn"},
  "inbounds": [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": %d}],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`, port)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, configPath) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run exited before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("mixed inbound never came up on port %d", port)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunMissingConfig(t *testing.T) {
	err := Run(context.Background(), filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a not-exist error, got: %v", err)
	}
}
