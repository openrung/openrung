//go:build !windows

package client

import (
	"context"
	"os"
	"path/filepath"
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
