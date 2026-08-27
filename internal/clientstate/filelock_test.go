//go:build !windows

package clientstate

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlink at the lock path must not be followed: an elevated run chmods and
// chowns the descriptor it opens, so following one would hand the invoking
// user whatever they aimed it at.
func TestWithFileLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("privileged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, proxyPortLockFile)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	called := false
	if err := withFileLock(link, func() error { called = true; return nil }); err == nil {
		t.Fatal("withFileLock followed a symlinked lock path")
	}
	if called {
		t.Fatal("locked body ran on a symlinked lock path")
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode = %v, want unchanged 0644", info.Mode().Perm())
	}
}

func TestWithFileLockCreatesRealLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), proxyPortLockFile)
	called := false
	if err := withFileLock(path, func() error { called = true; return nil }); err != nil {
		t.Fatalf("withFileLock: %v", err)
	}
	if !called {
		t.Fatal("locked body did not run")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v, want 0600", info.Mode().Perm())
	}
}
