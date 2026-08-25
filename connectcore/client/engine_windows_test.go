//go:build windows

package client

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The parent-death test needs three processes — the test (grandparent), a
// supervisor running the real SingBoxRunner (parent), and a stop-protocol
// child — so TestMain diverts re-execs of this binary into their roles before
// the test framework runs. Roles travel by environment because the runner owns
// the child's argv.
func TestMain(m *testing.M) {
	switch os.Getenv("OPENRUNG_TEST_PARENTDEATH_ROLE") {
	case "parent":
		parentDeathParentRole()
	case "child":
		parentDeathChildRole()
	default:
		os.Exit(m.Run())
	}
}

// parentDeathParentRole supervises a stop-protocol child through the REAL
// runner and then just waits to be terminated. Run blocks until the child
// exits, which only happens once the grandparent kills this process and the
// child sees its stdin close.
func parentDeathParentRole() {
	os.Setenv("OPENRUNG_TEST_PARENTDEATH_ROLE", "child")
	exe, err := os.Executable()
	if err != nil {
		os.Exit(3)
	}
	runner := SingBoxRunner{Path: exe, StopOnStdinClose: true, KillGrace: 30 * time.Second}
	_ = runner.Run(context.Background(), filepath.Join(os.Getenv("OPENRUNG_TEST_PARENTDEATH_DIR"), "config.json"))
	os.Exit(0)
}

// parentDeathChildRole emulates the bundled runtime's stop protocol: mark
// startup, block on stdin, and mark the graceful exit that only happens if
// nothing terminated this process first.
func parentDeathChildRole() {
	dir := os.Getenv("OPENRUNG_TEST_PARENTDEATH_DIR")
	if !strings.Contains(strings.Join(os.Args, " "), "-"+StopOnStdinCloseFlag) {
		os.Exit(2) // the runner failed to pass the protocol flag
	}
	if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0o644); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := os.WriteFile(filepath.Join(dir, "graceful"), nil, 0o644); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// TestParentDeathLetsTheStopProtocolChildExitGracefully proves, on real
// Windows, the property the stop pipe exists for: TerminateProcess on the
// supervising process — what Stop-Process and Task Manager do — leaves the
// stop-protocol child free to observe its stdin EOF and exit on its own
// terms. A kill-on-close job object holding that child would fail this test:
// the kernel closes the dying parent's handles in no defined order, and
// closing the job's last handle terminates the child immediately, before the
// EOF can reach it.
func TestParentDeathLetsTheStopProtocolChildExitGracefully(t *testing.T) {
	dir := t.TempDir()
	parent := exec.Command(os.Args[0])
	parent.Env = append(os.Environ(),
		"OPENRUNG_TEST_PARENTDEATH_ROLE=parent",
		"OPENRUNG_TEST_PARENTDEATH_DIR="+dir,
	)
	if err := parent.Start(); err != nil {
		t.Fatalf("start parent: %v", err)
	}
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		_, _ = parent.Process.Wait()
	})

	waitForMarker(t, filepath.Join(dir, "started"), 15*time.Second,
		"the stop-protocol child never started under the runner")

	// The whole point: kill the SUPERVISOR, not the child.
	if err := parent.Process.Kill(); err != nil {
		t.Fatalf("terminate parent: %v", err)
	}
	_, _ = parent.Process.Wait()

	waitForMarker(t, filepath.Join(dir, "graceful"), 10*time.Second,
		"the child did not exit gracefully after its supervisor died — something terminated it before the stdin EOF reached it (a kill-on-close job object racing the stop pipe?)")
}

func waitForMarker(t *testing.T, path string, timeout time.Duration, failure string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
