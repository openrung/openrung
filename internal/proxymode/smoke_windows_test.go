//go:build windows

// SPDX-License-Identifier: GPL-3.0-or-later

package proxymode

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// TestWindowsRealProxySettings drives the real WinInet settings, which the fake
// backend in windows_controller_test.go cannot stand in for. Every check reads
// what WinInet stored in the registry: the flags in the
// DefaultConnectionSettings blob that browsers resolve the proxy from, and the
// legacy values older readers use.
//
// It rewrites the user's proxy setting (restoring it afterwards), so it only
// runs when OPENRUNG_PROXYMODE_SMOKE=windows, as on CI's throwaway runner.
func TestWindowsRealProxySettings(t *testing.T) {
	if os.Getenv("OPENRUNG_PROXYMODE_SMOKE") != "windows" {
		t.Skip("set OPENRUNG_PROXYMODE_SMOKE=windows on a throwaway machine to run")
	}
	c := New().(*windowsController)
	original, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot of the original setting: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Restore(original); err != nil {
			t.Errorf("put back the original setting: %v", err)
		}
	})

	// A prior upstream proxy with a setup script and automatic detection on.
	on := true
	prior := WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   "upstream.example:3128",
		ProxyOverride: "<local>",
		AutoConfigURL: "http://wpad.example/proxy.pac",
		AutoDetect:    &on,
	}
	const priorFlags = proxyTypeDirect | proxyTypeProxy | proxyTypeAutoProxyURL | proxyTypeAutoDetect
	if err := c.Restore(Snapshot{Platform: "windows", Windows: &prior}); err != nil {
		t.Fatalf("seed the prior setting: %v", err)
	}
	checkStoredProxy(t, "seeded", priorFlags, prior)

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Windows; got == nil || got.AutoDetect == nil || !*got.AutoDetect ||
		got.ProxyServer != prior.ProxyServer || got.AutoConfigURL != prior.AutoConfigURL {
		t.Fatalf("Snapshot = %+v, want %+v", got, prior)
	}
	snap = roundTripJSON(t, snap)

	if err := c.Set("127.0.0.1", 7890); err != nil {
		t.Fatalf("Set: %v", err)
	}
	checkStoredProxy(t, "connected", proxyTypeDirect|proxyTypeProxy, WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   "127.0.0.1:7890",
		ProxyOverride: defaultProxyBypass,
	})

	if err := c.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	checkStoredProxy(t, "restored", priorFlags, prior)
}

func checkStoredProxy(t *testing.T, stage string, wantFlags uint32, want WindowsProxyState) {
	t.Helper()
	conns, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("%s: open Connections: %v", stage, err)
	}
	defer conns.Close()
	blob, _, err := conns.GetBinaryValue("DefaultConnectionSettings")
	if err != nil || len(blob) < 12 {
		t.Fatalf("%s: read DefaultConnectionSettings (%d bytes): %v", stage, len(blob), err)
	}
	if flags := binary.LittleEndian.Uint32(blob[8:12]); flags != wantFlags {
		t.Errorf("%s: connection flags = %#x, want %#x", stage, flags, wantFlags)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("%s: open Internet Settings: %v", stage, err)
	}
	defer key.Close()
	enable, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil {
		t.Errorf("%s: read ProxyEnable: %v", stage, err)
	} else if (enable != 0) != want.ProxyEnable {
		t.Errorf("%s: ProxyEnable = %d, want %v", stage, enable, want.ProxyEnable)
	}
	for name, value := range map[string]string{
		"ProxyServer":   want.ProxyServer,
		"ProxyOverride": want.ProxyOverride,
		"AutoConfigURL": want.AutoConfigURL,
	} {
		got, _, err := key.GetStringValue(name)
		if err != nil && !errors.Is(err, registry.ErrNotExist) {
			t.Errorf("%s: read %s: %v", stage, name, err)
		} else if got != value {
			t.Errorf("%s: %s = %q, want %q", stage, name, got, value)
		}
	}
}
