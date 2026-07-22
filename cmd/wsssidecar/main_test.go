package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/internal/wssbridge"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func configEnv(overrides map[string]string) func(string) string {
	values := map[string]string{
		"OPENRUNG_WSS_RELAY_ID":            "relay-a",
		"OPENRUNG_WSS_TICKET_PUBLIC_KEYS":  "not-decoded-until-serve",
		"OPENRUNG_WSS_FRONT_ORIGIN_TOKENS": `{"front-a":["origin-token-that-is-at-least-32-bytes"]}`,
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

func TestParseConfigDefaultsAreRelayLocalAndCGNATFriendly(t *testing.T) {
	cfg, err := parseConfig(nil, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelayID != "relay-a" || cfg.FixedTarget != wssbridge.DefaultFixedTarget || cfg.Addr != defaultAddr {
		t.Fatalf("relay-local defaults = %+v", cfg)
	}
	if cfg.ReplayStatePath != defaultReplayStatePath || !strings.HasPrefix(cfg.ReplayStatePath, "/") {
		t.Fatalf("durable replay state default = %q", cfg.ReplayStatePath)
	}
	if cfg.MaxSessionsPerSource != wssbridge.DefaultMaxSessionsPerSource || cfg.MaxSessionsPerSource < 100 {
		t.Fatalf("max sessions per source = %d", cfg.MaxSessionsPerSource)
	}
	if cfg.GlobalHandshakeRate != wssbridge.DefaultGlobalHandshakeRatePerSecond || cfg.GlobalHandshakeBurst != wssbridge.DefaultGlobalHandshakeBurst {
		t.Fatalf("global handshake defaults = %v/%d", cfg.GlobalHandshakeRate, cfg.GlobalHandshakeBurst)
	}
	if cfg.MaxPendingHandshakes != wssbridge.DefaultMaxPendingHandshakes {
		t.Fatalf("max pending handshakes = %d", cfg.MaxPendingHandshakes)
	}
	if len(cfg.FrontOriginTokens["front-a"]) != 1 {
		t.Fatalf("front tokens = %+v", cfg.FrontOriginTokens)
	}
}

func TestParseConfigRejectsNonLoopbackListenerAndTarget(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"listener": {"OPENRUNG_WSS_ADDR": "0.0.0.0:8081"},
		"target":   {"OPENRUNG_WSS_FIXED_TARGET": "192.0.2.1:443"},
		"fronts":   {"OPENRUNG_WSS_FRONT_ORIGIN_TOKENS": `{}`},
		"replay":   {"OPENRUNG_WSS_REPLAY_STATE": "relative/replay.journal"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(nil, configEnv(env)); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestParseConfigFlagsOverrideBoundedControls(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-fixed-target", "[::1]:4443",
		"-max-sessions-per-source", "700",
		"-max-streams-per-source", "5000",
		"-max-pending-handshakes", "900",
		"-global-handshake-rate", "3000",
		"-global-handshake-burst", "12000",
		"-session-lifetime", "2h",
		"-replay-state", "/var/lib/openrung/custom-replay.journal",
	}, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FixedTarget != "[::1]:4443" || cfg.MaxSessionsPerSource != 700 || cfg.MaxStreamsPerSource != 5000 || cfg.MaxPendingHandshakes != 900 || cfg.GlobalHandshakeRate != 3000 || cfg.GlobalHandshakeBurst != 12000 || cfg.SessionLifetime != 2*time.Hour || cfg.ReplayStatePath != "/var/lib/openrung/custom-replay.journal" {
		t.Fatalf("flag overrides = %+v", cfg)
	}
}

func TestStatsLoggingContainsOnlyFixedAggregateFields(t *testing.T) {
	var output lockedBuffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		logSidecarStats(ctx, logger, &wssbridge.SidecarStats{}, time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for output.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	logged := output.String()
	if !strings.Contains(logged, "accepted_sessions") || !strings.Contains(logged, "dial_failures") {
		t.Fatalf("aggregate log = %q", logged)
	}
	for _, forbidden := range []string{"relay-a", "front-a", "127.0.0.1", "origin-token-value", "secret-ticket-value"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("aggregate log contains forbidden label/data %q: %s", forbidden, logged)
		}
	}
}

func TestVersionInfoUsesSidecarTerminology(t *testing.T) {
	if got := versionInfo(); !strings.HasPrefix(got, "wsssidecar/") {
		t.Fatalf("version info = %q", got)
	}
}
