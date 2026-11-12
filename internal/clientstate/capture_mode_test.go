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
	// No choice on disk leaves the starting mode to the app.
	if tun, stored := store.LoadDesktopTUNMode(); tun || stored {
		t.Fatalf("fresh install: tun = %v, stored = %v", tun, stored)
	}
	if err := store.SaveDesktopTUNMode(true); err != nil {
		t.Fatal(err)
	}
	if tun, stored := NewInDir(dir).LoadDesktopTUNMode(); !tun || !stored {
		t.Fatalf("TUN mode did not survive a restart: tun = %v, stored = %v", tun, stored)
	}
	if err := store.SaveDesktopTUNMode(false); err != nil {
		t.Fatal(err)
	}
	// Proxy mode is a stored choice, distinct from no choice.
	if tun, stored := store.LoadDesktopTUNMode(); tun || !stored {
		t.Fatalf("proxy mode: tun = %v, stored = %v", tun, stored)
	}
	// Anything unrecognized is no choice at all.
	if err := os.WriteFile(filepath.Join(dir, desktopCaptureModeFile), []byte("TUN!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tun, stored := store.LoadDesktopTUNMode(); tun || stored {
		t.Fatalf("unrecognized value: tun = %v, stored = %v", tun, stored)
	}
}
