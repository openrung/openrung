//go:build !windows

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
	origGeteuid, origFileChown := geteuid, fileChown
	geteuid = func() int { return euid }
	fileChown = func(file *os.File, uid, gid int) error {
		calls = append(calls, chownCall{path: file.Name(), uid: uid, gid: gid})
		return nil
	}
	t.Cleanup(func() { geteuid, fileChown = origGeteuid, origFileChown })
	return &calls
}

func openTestFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "state-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestChownUnderSudoUsesInvokingUser(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	if !Active() {
		t.Fatal("Active() = false for root with SUDO_UID/SUDO_GID set")
	}
	file := openTestFile(t)
	if err := ChownFile(file); err != nil {
		t.Fatalf("ChownFile: %v", err)
	}
	want := []chownCall{{path: file.Name(), uid: 501, gid: 20}}
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
	if err := ChownFile(openTestFile(t)); err != nil {
		t.Fatalf("ChownFile: %v", err)
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
	if err := ChownFile(openTestFile(t)); err != nil {
		t.Fatalf("ChownFile: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestChownIgnoresMalformedSudoIDs(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "not-a-number")
	t.Setenv("SUDO_GID", "20")

	if err := ChownFile(openTestFile(t)); err != nil {
		t.Fatalf("ChownFile: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestMkdirAllChownsOnlyCreatedDirs(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	base := canonicalTempDir(t) // pre-existing: must not be chowned
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

	dir := canonicalTempDir(t)
	if err := MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none for an existing directory", *calls)
	}
}

func TestMkdirAllRejectsSymlinkComponent(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	base := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	link := filepath.Join(base, "config")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	err := MkdirAll(filepath.Join(link, "openrung"), 0o700)
	if err == nil {
		t.Fatal("MkdirAll followed a symlink component")
	}
	if _, err := os.Stat(filepath.Join(outside, "openrung")); !os.IsNotExist(err) {
		t.Fatalf("outside directory was modified through symlink: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestOpenRegularFileRejectsSymlinks(t *testing.T) {
	base := canonicalTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "client-id")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if file, err := OpenRegularFile(link, os.O_WRONLY|os.O_TRUNC, 0); err == nil {
		file.Close()
		t.Fatal("OpenRegularFile followed a final symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("target = %q, want unchanged", data)
	}
}

func TestChownRegularFileAtRejectsSymlink(t *testing.T) {
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", "501")
	t.Setenv("SUDO_GID", "20")

	base := canonicalTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "client-id")); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(base)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if err := ChownRegularFileAt(dir, "client-id"); err == nil {
		t.Fatal("ChownRegularFileAt followed a symlink")
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}
