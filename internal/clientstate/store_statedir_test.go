//go:build !windows

package clientstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openrung/openrung/connectcore/sudouser"
)

// A Store rooted at a pinned directory must write there and nowhere else, even
// once the directory's pathname has been swapped for a symlink into a
// privileged tree — proxy-env-*.sh landing in /etc/profile.d would be a script
// root sources at login.
func TestStoreWritesThroughPinnedDirOnly(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir, err := sudouser.OpenStateDir(dir)
	if err != nil {
		t.Fatalf("OpenStateDir: %v", err)
	}
	defer stateDir.Close()
	store := &Store{dir: dir, stateDir: stateDir}

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

	if _, err := store.SaveProxyEnvScript(1080, []byte("# helper\n")); err != nil {
		t.Fatalf("SaveProxyEnvScript: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privileged, "proxy-env-1080.sh")); err == nil {
		t.Fatal("wrote a script into the privileged directory through the symlink")
	}
	if _, err := os.Stat(filepath.Join(base, "openrung.real", "proxy-env-1080.sh")); err != nil {
		t.Fatalf("write did not land in the pinned directory: %v", err)
	}
}

// The pinned path must stay byte-compatible with the pathname path, since the
// desktop app and the TUI share these files.
func TestPinnedWritesMatchUnpinnedFormat(t *testing.T) {
	recents := []RecentNode{{CountryCode: "KR", Label: "Seoul", Latitude: 37.5, Longitude: 127.0}}

	plain := t.TempDir()
	if err := NewInDir(plain).SaveRecents(recents); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(plain, recentsFile))
	if err != nil {
		t.Fatal(err)
	}

	pinnedDir := t.TempDir()
	stateDir, err := sudouser.OpenStateDir(pinnedDir)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDir.Close()
	pinned := &Store{dir: pinnedDir, stateDir: stateDir}
	if err := pinned.SaveRecents(recents); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(pinnedDir, recentsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("pinned write = %q, want %q", got, want)
	}
	if loaded := pinned.LoadRecents(); len(loaded) != 1 || loaded[0] != recents[0] {
		t.Fatalf("LoadRecents = %v, want %v", loaded, recents)
	}
	info, err := os.Stat(filepath.Join(pinnedDir, recentsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
