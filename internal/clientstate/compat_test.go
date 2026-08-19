package clientstate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"openrung/internal/proxymode"
)

// The fixtures under testdata/ are a config directory as the desktop app wrote
// it before these primitives moved out of desktop/persist (ADR-001 PR A3).
// A user upgrading across that move keeps their directory, so the directory
// name, the file names, and the JSON encodings are a compatibility contract:
// the current build must load them, and must write them back byte for byte.

func TestLoadsConfigDirectoryWrittenByThePreviousBuild(t *testing.T) {
	store := NewInDir(filepath.Join("testdata", "config-dir"))

	recents := store.LoadRecents()
	want := []RecentNode{
		{CountryCode: "JP", Label: "Tokyo, Japan", Latitude: 35.6895, Longitude: 139.6917},
		{CountryCode: "DE", Label: "Falkenstein, Germany", Latitude: 50.4779, Longitude: 12.3713},
	}
	if len(recents) != len(want) {
		t.Fatalf("LoadRecents = %+v, want %+v", recents, want)
	}
	for i := range want {
		if recents[i] != want[i] {
			t.Fatalf("recents[%d] = %+v, want %+v", i, recents[i], want[i])
		}
	}

	port, ok := store.LoadProxyPort()
	if !ok || port != 46685 {
		t.Fatalf("LoadProxyPort = %d, %v; want 46685, true", port, ok)
	}

	snap, ok := store.LoadProxySnapshot()
	if !ok || snap.Platform != "darwin" || snap.Windows != nil || len(snap.Services) != 2 {
		t.Fatalf("LoadProxySnapshot = %+v, ok=%v", snap, ok)
	}
	wantService := proxymode.ServiceProxyState{
		Name: "Wi-Fi", WebEnabled: true, WebHost: "10.0.0.1", WebPort: 3128,
		SecureEnabled: true, SecureHost: "10.0.0.1", SecurePort: 3128,
	}
	if snap.Services[0] != wantService {
		t.Fatalf("services[0] = %+v, want %+v", snap.Services[0], wantService)
	}
	if snap.Services[1] != (proxymode.ServiceProxyState{Name: "Thunderbolt Bridge"}) {
		t.Fatalf("services[1] = %+v, want a disabled Thunderbolt Bridge", snap.Services[1])
	}

	// A Windows snapshot carries the global WinInet capture instead of the
	// per-service list; both shapes share one file name.
	windows, ok := NewInDir(filepath.Join("testdata", "config-dir-windows")).LoadProxySnapshot()
	if !ok || windows.Platform != "windows" || windows.Services != nil || windows.Windows == nil {
		t.Fatalf("windows snapshot = %+v, ok=%v", windows, ok)
	}
	wantWindows := proxymode.WindowsProxyState{
		ProxyEnable: true, ProxyServer: "10.0.0.1:3128",
		ProxyOverride: "localhost;127.*;<local>", AutoConfigURL: "http://wpad.example/wpad.dat",
	}
	if *windows.Windows != wantWindows {
		t.Fatalf("windows capture = %+v, want %+v", *windows.Windows, wantWindows)
	}
}

func TestRewritesConfigDirectoryByteIdentically(t *testing.T) {
	// Loading is only half of it: a build that reads the old files but writes a
	// different encoding would break the previous build on a downgrade, and
	// would silently churn the directory. Round-trip every file through the
	// current writers and compare bytes with the fixture.
	fixtureDir := filepath.Join("testdata", "config-dir")
	fixture := NewInDir(fixtureDir)
	store := NewInDir(t.TempDir())

	if err := store.SaveRecents(fixture.LoadRecents()); err != nil {
		t.Fatalf("SaveRecents: %v", err)
	}
	port, _ := fixture.LoadProxyPort()
	if err := store.SaveProxyPort(port); err != nil {
		t.Fatalf("SaveProxyPort: %v", err)
	}
	snap, _ := fixture.LoadProxySnapshot()
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("SaveProxySnapshot: %v", err)
	}

	for _, name := range []string{recentsFile, proxyPortFile, proxySnapshotHdr} {
		want, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(store.dir, name))
		if err != nil {
			t.Fatalf("read rewritten %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s was rewritten in a different encoding:\ngot:\n%s\nwant:\n%s", name, got, want)
		}
	}

	// The shell helper is regenerated every launch, so only its port-qualified
	// name is a contract — an unqualified name would let two instances fight
	// over one file.
	helper, err := store.SaveProxyEnvScript(port, []byte("# helper\n"))
	if err != nil {
		t.Fatalf("SaveProxyEnvScript: %v", err)
	}
	if got, want := filepath.Base(helper), fmt.Sprintf("proxy-env-%d.sh", port); got != want {
		t.Fatalf("shell helper name = %q, want %q", got, want)
	}
}

func TestNewResolvesTheOpenRungConfigDirectory(t *testing.T) {
	// The directory name is part of the contract: the desktop app, the TUI
	// client, and the telemetry client-id all share os.UserConfigDir()/openrung.
	// Point os.UserConfigDir at a temp root rather than the real one so the test
	// never touches the developer's own state.
	base := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", base)
	case "darwin", "ios":
		t.Setenv("HOME", base)
	default:
		t.Setenv("XDG_CONFIG_HOME", base)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	store, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if want := filepath.Join(configDir, "openrung"); store.dir != want {
		t.Fatalf("config dir = %q, want %q", store.dir, want)
	}
	info, err := os.Stat(store.dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("config dir was not created: %v", err)
	}
}
