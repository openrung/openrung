// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopTUNModeRoundTrips(t *testing.T) {
	dir := t.TempDir()
	store := NewInDir(dir)
	if store.LoadDesktopTUNMode() {
		t.Fatal("a fresh install must start in proxy mode")
	}
	if err := store.SaveDesktopTUNMode(true); err != nil {
		t.Fatal(err)
	}
	if !NewInDir(dir).LoadDesktopTUNMode() {
		t.Fatal("TUN mode did not survive a restart")
	}
	if err := store.SaveDesktopTUNMode(false); err != nil {
		t.Fatal(err)
	}
	if store.LoadDesktopTUNMode() {
		t.Fatal("proxy mode did not replace the stored TUN mode")
	}
	// Anything unrecognized is the mode that needs no privileges.
	if err := os.WriteFile(filepath.Join(dir, desktopCaptureModeFile), []byte("TUN!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.LoadDesktopTUNMode() {
		t.Fatal("an unrecognized value was read as TUN mode")
	}
}
