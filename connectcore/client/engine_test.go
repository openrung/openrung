//go:build !windows

package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunKillGraceBoundsCancelTeardown proves a cancelled Run returns promptly
// even when the child ignores the interrupt: the configurable grace (used by
// the desktop connect ladder) falls back to a hard kill.
func TestRunKillGraceBoundsCancelTeardown(t *testing.T) {
	dir := t.TempDir()
	// A stand-in "sing-box" that ignores SIGINT; only the post-grace hard kill
	// can end it. It receives "run -c <config>" like the real binary.
	script := filepath.Join(dir, "stubbox")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' INT\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	runner := SingBoxRunner{Path: script, KillGrace: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, config) }()

	time.Sleep(100 * time.Millisecond) // let the child start
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled run should return nil, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancel; KillGrace not honored")
	}
}

// TestRunCrashErrorQuotesTheChildsOwnWords proves an abnormal exit carries the
// child's error line, not just "exit status 1": that line is what reaches the
// user-facing Error row, and a bare exit status once hid a missing with_utls
// build tag the child had spelled out on stderr.
func TestRunCrashErrorQuotesTheChildsOwnWords(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\n" +
		"echo 'INFO some benign startup chatter'\n" +
		"echo 'error: create sing-box service: uTLS is not included in this build' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var logged strings.Builder
	runner := SingBoxRunner{Path: script, Stdout: &logged, Stderr: &logged}
	err := runner.Run(context.Background(), config)
	if err == nil {
		t.Fatal("crashing run returned nil")
	}
	for _, want := range []string{"sing-box exited", "uTLS is not included in this build", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("crash error %q missing %q", err, want)
		}
	}
	// The tee must still deliver the output to the engine log writers.
	if !strings.Contains(logged.String(), "uTLS is not included") || !strings.Contains(logged.String(), "benign startup chatter") {
		t.Fatalf("recorder swallowed output meant for the log: %q", logged.String())
	}
}

// A crash with no error-looking line still quotes the last thing the child
// said — even without a trailing newline.
func TestRunCrashErrorFallsBackToLastLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'giving up on relay handshake' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SingBoxRunner{Path: script}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "giving up on relay handshake") {
		t.Fatalf("crash error did not quote the unterminated last line: %v", err)
	}
}
