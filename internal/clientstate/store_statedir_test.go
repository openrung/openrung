//go:build !windows

package clientstate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/openrung/openrung/connectcore/sudouser"

	"openrung/internal/proxymode"
)

// pinnedStore builds a Store holding a validated directory descriptor, the
// shape New produces under sudo.
func pinnedStore(t *testing.T, dir string) *Store {
	t.Helper()
	stateDir, err := sudouser.OpenStateDir(dir)
	if err != nil {
		t.Fatalf("OpenStateDir: %v", err)
	}
	t.Cleanup(func() { _ = stateDir.Close() })
	return &Store{dir: dir, stateDir: stateDir}
}

// swapForSymlink replaces dir's pathname with a symlink to target, the move an
// attacker makes after New has validated the directory.
func swapForSymlink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestClearProxySnapshotStaysInPinnedDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := pinnedStore(t, dir)
	if err := store.SaveProxySnapshot(proxymode.Snapshot{}); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, proxySnapshotHdr)
	if err := os.WriteFile(victim, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapForSymlink(t, dir, outside)

	if err := store.ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("deleted a file outside the pinned directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir+".real", proxySnapshotHdr)); !os.IsNotExist(err) {
		t.Fatalf("snapshot in the pinned directory was not removed: %v", err)
	}
}

func TestClearProxySnapshotMissingIsNotAnError(t *testing.T) {
	if err := pinnedStore(t, t.TempDir()).ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot on a missing snapshot: %v", err)
	}
}

func TestPinnedWritesAreConcurrencySafe(t *testing.T) {
	// Concurrent refreshes of the same state file must not share a temporary
	// name, or they truncate and rename each other's work.
	store := pinnedStore(t, t.TempDir())
	const writers = 16

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			port := 1080 + i
			if _, err := store.SaveProxyEnvScript(port, []byte(fmt.Sprintf("# %d\n", port))); err != nil {
				errs <- err
			}
			if err := store.SaveProxySnapshot(proxymode.Snapshot{}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}

	for i := 0; i < writers; i++ {
		port := 1080 + i
		data, err := os.ReadFile(filepath.Join(store.dir, fmt.Sprintf(proxyEnvFile, port)))
		if err != nil {
			t.Fatalf("helper for port %d: %v", port, err)
		}
		if string(data) != fmt.Sprintf("# %d\n", port) {
			t.Fatalf("helper for port %d = %q, want its own content", port, data)
		}
	}
	// No temporary files may survive a clean run.
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".sh" && entry.Name() != proxySnapshotHdr {
			t.Errorf("leftover file %q", entry.Name())
		}
	}
}

func TestPinnedReadsDoNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "openrung")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.json")
	if err := os.WriteFile(outside, []byte(`[{"countryCode":"XX","label":"leak"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, recentsFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if recents := pinnedStore(t, dir).LoadRecents(); len(recents) != 0 {
		t.Fatalf("LoadRecents read through a symlink: %v", recents)
	}
}

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
