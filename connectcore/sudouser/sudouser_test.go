package sudouser

import (
	"os"
	"path/filepath"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

// stubElevation fakes the effective uid and records chown calls, restoring
// the real syscalls when the test ends.
func stubElevation(t *testing.T, euid int) *[]chownCall {
	t.Helper()
	var calls []chownCall
	origGeteuid, origChown := geteuid, chown
	geteuid = func() int { return euid }
	chown = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path: path, uid: uid, gid: gid})
		return nil
	}
	t.Cleanup(func() { geteuid, chown = origGeteuid, origChown })
	return &calls
}

func TestChownUnderSudoUsesInvokingUser(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	if !Active() {
		t.Fatal("Active() = false for root with SUDO_UID/SUDO_GID set")
	}
	if err := Chown("/some/path"); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	want := []chownCall{{path: "/some/path", uid: 501, gid: 20}}
	if len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
}

func TestChownNoopWhenNotRoot(t *testing.T) {
	// A non-root process must never chown, even with sudo variables present
	// (e.g. a plain child of a sudo'd shell).
	calls := stubElevation(t, 501)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	if Active() {
		t.Fatal("Active() = true for a non-root process")
	}
	if err := Chown("/some/path"); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestChownNoopForGenuineRootDaemon(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "")
	t.Setenv("SUDO_GID", "")

	if Active() {
		t.Fatal("Active() = true for root without SUDO_UID")
	}
	if err := Chown("/some/path"); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestChownIgnoresMalformedSudoIDs(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "not-a-number")
	t.Setenv("SUDO_GID", "20")

	if err := Chown("/some/path"); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestMkdirAllChownsOnlyCreatedDirs(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	base := t.TempDir() // pre-existing: must not be chowned
	target := filepath.Join(base, "config", "openrung")

	if err := MkdirAll(target, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("target directory missing after MkdirAll: %v", err)
	}

	want := []chownCall{
		{path: filepath.Join(base, "config"), uid: 501, gid: 20},
		{path: target, uid: 501, gid: 20},
	}
	if len(*calls) != len(want) {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("chown call %d = %v, want %v", i, (*calls)[i], want[i])
		}
	}
}

func TestMkdirAllExistingDirChownsNothing(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	dir := t.TempDir()
	if err := MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none for an existing directory", *calls)
	}
}
