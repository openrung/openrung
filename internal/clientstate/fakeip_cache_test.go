// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFakeIPCacheLocationSharedByHosts(t *testing.T) {
	state := t.TempDir()
	dir, name, err := fakeIPCacheLocation(state)
	if err != nil {
		t.Fatal(err)
	}
	dir2, name2, err := fakeIPCacheLocation(state)
	if err != nil || dir != dir2 || name != name2 {
		t.Fatalf("unstable cache path: %s/%s vs %s/%s: %v", dir, name, dir2, name2, err)
	}
	otherDir, otherName, err := fakeIPCacheLocation(t.TempDir())
	if err != nil || filepath.Join(dir, name) == filepath.Join(otherDir, otherName) {
		t.Fatalf("installations share a cache: %v", err)
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 && dir == state {
		t.Fatal("privileged cache is in the invoking user's state directory")
	}
}

func TestFakeIPCacheDirectoryPrivacy(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("do not create databases or directories in root's real home")
	}
	dir := filepath.Join(t.TempDir(), "private")
	first, err := NewInDir(dir).FakeIPCachePath()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewInDir(dir).FakeIPCachePath()
	if err != nil || first != second {
		t.Fatalf("hosts disagree: %s %s: %v", first, second, err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("host created the sing-box database")
	}
	if runtime.GOOS == "windows" {
		return
	} // Windows uses account ACLs.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInDir(dir).FakeIPCachePath(); err == nil {
		t.Fatal("accepted a non-private directory")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInDir(link).FakeIPCachePath(); err == nil {
		t.Fatal("accepted a symlinked directory")
	}
}
