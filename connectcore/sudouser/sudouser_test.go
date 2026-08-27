//go:build !windows

package sudouser

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// stubInvoker fakes an elevated process whose invoking user is the uid that
// actually owns the test's files, so the ownership gates behave as they would
// under a real sudo run.
func stubInvoker(t *testing.T) (*[]chownCall, int, int) {
	t.Helper()
	calls := stubElevation(t, 0)
	uid, gid := os.Getuid(), os.Getgid()
	t.Setenv("SUDO_UID", strconv.Itoa(uid))
	t.Setenv("SUDO_GID", strconv.Itoa(gid))
	return calls, uid, gid
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
	calls, uid, gid := stubInvoker(t)

	if !Active() {
		t.Fatal("Active() = false for root with SUDO_UID/SUDO_GID set")
	}
	file := openTestFile(t)
	if err := ChownFile(file); err != nil {
		t.Fatalf("ChownFile: %v", err)
	}
	want := []chownCall{{path: file.Name(), uid: uid, gid: gid}}
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
	calls, uid, gid := stubInvoker(t)

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
		{path: filepath.Join(base, "config"), uid: uid, gid: gid},
		{path: target, uid: uid, gid: gid},
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
	calls, _, _ := stubInvoker(t)

	dir := canonicalTempDir(t)
	if err := MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none for an existing directory", *calls)
	}
}

func TestMkdirAllTraversesSymlinkedAncestor(t *testing.T) {
	// A home directory on another volume, or ~/.config pointing into a
	// dotfiles checkout, must not break an elevated run.
	calls, uid, gid := stubInvoker(t)

	base := canonicalTempDir(t)
	real := filepath.Join(base, "dotfiles")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "config")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	target := filepath.Join(link, "openrung")
	if err := MkdirAll(target, 0o700); err != nil {
		t.Fatalf("MkdirAll through a symlinked ancestor: %v", err)
	}
	if info, err := os.Stat(filepath.Join(real, "openrung")); err != nil || !info.IsDir() {
		t.Fatalf("directory not created through the symlink: %v", err)
	}
	// Only the created directory changes hands; the symlinked ancestor and
	// its target are left alone.
	want := []chownCall{{path: target, uid: uid, gid: gid}}
	if len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
}

func TestMkdirAllSkipsChownOutsideInvokersTree(t *testing.T) {
	// Standing in for ~/.config symlinked at a root-owned tree such as /etc:
	// the invoking user does not own the parent, so a directory created there
	// is not handed to them.
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()+1))
	t.Setenv("SUDO_GID", strconv.Itoa(os.Getgid()))

	target := filepath.Join(canonicalTempDir(t), "openrung")
	if err := MkdirAll(target, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none for a parent the invoker does not own", *calls)
	}
}

func TestOpenStateDirRejectsSymlinkedDir(t *testing.T) {
	calls, _, _ := stubInvoker(t)

	base := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	link := filepath.Join(base, "openrung")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if dir, err := OpenStateDir(link); err == nil {
		dir.Close()
		t.Fatal("OpenStateDir followed a symlinked state directory")
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestOpenStateDirFollowsSymlinkedParent(t *testing.T) {
	// ~/.config pointing into a dotfiles checkout must still be repairable;
	// only the state directory itself may not be a symlink.
	calls, uid, gid := stubInvoker(t)

	base := canonicalTempDir(t)
	real := filepath.Join(base, "dotfiles")
	if err := os.MkdirAll(filepath.Join(real, "openrung"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "config")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	stateDir, err := OpenStateDir(filepath.Join(base, "config", "openrung"))
	if err != nil {
		t.Fatalf("OpenStateDir through a symlinked parent: %v", err)
	}
	defer stateDir.Close()
	if err := stateDir.RepairOwner(); err != nil {
		t.Fatalf("RepairOwner: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].uid != uid || (*calls)[0].gid != gid {
		t.Fatalf("chown calls = %v, want the state directory handed to %d:%d", *calls, uid, gid)
	}
}

func TestRepairOwnerSkipsTreeInvokerDoesNotOwn(t *testing.T) {
	// Standing in for ~/.config symlinked at a root-owned tree such as /etc.
	calls := stubElevation(t, 0)
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()+1))
	t.Setenv("SUDO_GID", strconv.Itoa(os.Getgid()))

	dir := filepath.Join(canonicalTempDir(t), "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir, err := OpenStateDir(dir)
	if err != nil {
		t.Fatalf("OpenStateDir: %v", err)
	}
	defer stateDir.Close()
	if err := stateDir.RepairOwner(); err != nil {
		t.Fatalf("RepairOwner: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none for a parent the invoker does not own", *calls)
	}
}

func TestOpenStateDirChownsRealDir(t *testing.T) {
	calls, uid, gid := stubInvoker(t)

	dir := filepath.Join(canonicalTempDir(t), "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir, err := OpenStateDir(dir)
	if err != nil {
		t.Fatalf("OpenStateDir: %v", err)
	}
	defer stateDir.Close()
	if err := stateDir.RepairOwner(); err != nil {
		t.Fatalf("RepairOwner: %v", err)
	}

	want := []chownCall{{path: dir, uid: uid, gid: gid}}
	if len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
}

func TestRepairEntryRejectsSymlink(t *testing.T) {
	calls, _, _ := stubInvoker(t)

	dir := filepath.Join(canonicalTempDir(t), "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "client-id")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	stateDir, err := OpenStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDir.Close()

	if err := stateDir.RepairEntry("client-id"); err == nil {
		t.Fatal("RepairEntry followed a symlink")
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestRepairEntryRejectsHardLink(t *testing.T) {
	// A hard link planted at a state path reaches a file the invoking user may
	// not own, and O_NOFOLLOW cannot see it.
	calls, _, _ := stubInvoker(t)

	dir := filepath.Join(canonicalTempDir(t), "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("privileged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "client-id")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	stateDir, err := OpenStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDir.Close()

	if err := stateDir.RepairEntry("client-id"); err == nil {
		t.Fatal("RepairEntry accepted a multiply-linked file")
	}
	if len(*calls) != 0 {
		t.Fatalf("chown calls = %v, want none", *calls)
	}
}

func TestOpenRegularFileValidatesBeforeTruncating(t *testing.T) {
	// O_TRUNC must not reach a file that is about to be rejected: truncation
	// is destructive, and rejecting afterwards is too late.
	stubInvoker(t)

	dir := canonicalTempDir(t)
	target := filepath.Join(dir, "victim")
	content := []byte("privileged content\n")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "client-id")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	file, err := OpenRegularFile(filepath.Join(dir, "client-id"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err == nil {
		file.Close()
		t.Fatal("OpenRegularFile returned a descriptor to a multiply-linked file")
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(content) {
		t.Fatalf("target = %q, want unchanged %q", data, content)
	}
}

func TestOpenRegularFileStillTruncates(t *testing.T) {
	// The withheld O_TRUNC must still be applied once the file checks out.
	path := filepath.Join(canonicalTempDir(t), "client-id")
	if err := os.WriteFile(path, []byte("stale-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenRegularFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("OpenRegularFile: %v", err)
	}
	if _, err := file.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("file = %q, want %q (truncation not applied)", data, "new\n")
	}
}

func TestStateDirWriteFileStaysInPinnedDir(t *testing.T) {
	// Swapping the directory's pathname for a symlink after it was validated
	// must not redirect the write.
	stubInvoker(t)

	base := canonicalTempDir(t)
	dir := filepath.Join(base, "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir, err := OpenStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDir.Close()

	privileged := filepath.Join(base, "profile.d")
	if err := os.Mkdir(privileged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(base, "openrung.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(privileged, dir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := stateDir.WriteFile("proxy-env-1080.sh", []byte("# helper\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privileged, "proxy-env-1080.sh")); err == nil {
		t.Fatal("write followed the swapped-in symlink")
	}
	if _, err := os.Stat(filepath.Join(base, "openrung.real", "proxy-env-1080.sh")); err != nil {
		t.Fatalf("write did not land in the pinned directory: %v", err)
	}
}

func TestOpenRegularFileDoesNotBlockOnFifo(t *testing.T) {
	// Without O_NONBLOCK, opening a FIFO for writing parks in open(2) until a
	// reader arrives, hanging client startup instead of failing.
	base := canonicalTempDir(t)
	path := filepath.Join(base, "client-id")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		file, err := OpenRegularFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("OpenRegularFile accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenRegularFile blocked on a FIFO")
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
