package proxymode

import (
	"errors"
	"strings"
	"testing"
)

// fakeBackend is an in-memory winProxyBackend: it records writes and notifies so
// the controller logic can be tested without a real registry. This test has no
// build tag, so it runs on every platform (including the macOS dev machine).
type fakeBackend struct {
	state    WindowsProxyState
	readErr  error
	writes   []WindowsProxyState
	notifies int
}

func (f *fakeBackend) read() (WindowsProxyState, error) {
	if f.readErr != nil {
		return WindowsProxyState{}, f.readErr
	}
	return f.state, nil
}

func (f *fakeBackend) write(s WindowsProxyState) error {
	f.writes = append(f.writes, s)
	f.state = s
	return nil
}

func (f *fakeBackend) notify() error {
	f.notifies++
	return nil
}

func TestWindowsSnapshotCapturesState(t *testing.T) {
	backend := &fakeBackend{state: WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   "10.0.0.1:3128",
		ProxyOverride: "<local>",
		AutoConfigURL: "http://wpad/proxy.pac",
	}}
	c := &windowsController{backend: backend}

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Platform != "windows" {
		t.Fatalf("platform = %q", snap.Platform)
	}
	if snap.Windows == nil {
		t.Fatal("expected a Windows capture")
	}
	if *snap.Windows != backend.state {
		t.Fatalf("snapshot state = %+v, want %+v", *snap.Windows, backend.state)
	}
}

func TestWindowsSetEnablesLoopbackProxy(t *testing.T) {
	backend := &fakeBackend{}
	c := &windowsController{backend: backend}

	if err := c.Set("127.0.0.1", 7890); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(backend.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(backend.writes))
	}
	got := backend.writes[0]
	if !got.ProxyEnable {
		t.Error("ProxyEnable should be true")
	}
	if got.ProxyServer != "127.0.0.1:7890" {
		t.Errorf("ProxyServer = %q, want 127.0.0.1:7890", got.ProxyServer)
	}
	if got.AutoConfigURL != "" {
		t.Errorf("AutoConfigURL = %q, want empty (PAC cleared)", got.AutoConfigURL)
	}
	// Loopback and LAN must bypass the proxy so 127.0.0.1 traffic can't loop back
	// into sing-box and local resources stay direct.
	for _, want := range []string{"127.*", "192.168.*", "<local>"} {
		if !strings.Contains(got.ProxyOverride, want) {
			t.Errorf("ProxyOverride %q missing %q", got.ProxyOverride, want)
		}
	}
	if backend.notifies != 1 {
		t.Errorf("notifies = %d, want 1", backend.notifies)
	}
}

func TestWindowsRestoreReappliesSnapshot(t *testing.T) {
	original := WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   "10.0.0.1:3128",
		ProxyOverride: "<local>",
		AutoConfigURL: "http://wpad/proxy.pac",
	}
	backend := &fakeBackend{}
	c := &windowsController{backend: backend}

	if err := c.Restore(Snapshot{Platform: "windows", Windows: &original}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(backend.writes) != 1 || backend.writes[0] != original {
		t.Fatalf("restore wrote %+v, want %+v", backend.writes, original)
	}
	if backend.notifies != 1 {
		t.Errorf("notifies = %d, want 1", backend.notifies)
	}
}

func TestWindowsRestoreNilCaptureDisablesProxy(t *testing.T) {
	backend := &fakeBackend{}
	c := &windowsController{backend: backend}

	// An empty snapshot (no Windows capture) should disable the proxy, undoing a
	// prior Set even when the original state was never recorded.
	if err := c.Restore(Snapshot{Platform: "windows"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(backend.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(backend.writes))
	}
	if backend.writes[0].ProxyEnable {
		t.Error("expected proxy disabled on nil-capture restore")
	}
}

func TestWindowsRestoreRejectsForeignPlatform(t *testing.T) {
	c := &windowsController{backend: &fakeBackend{}}
	if err := c.Restore(Snapshot{Platform: "darwin"}); err == nil {
		t.Fatal("expected refusal to restore a non-windows snapshot")
	}
}

func TestWindowsSnapshotPropagatesReadError(t *testing.T) {
	sentinel := errors.New("boom")
	c := &windowsController{backend: &fakeBackend{readErr: sentinel}}
	if _, err := c.Snapshot(); !errors.Is(err, sentinel) {
		t.Fatalf("Snapshot err = %v, want %v", err, sentinel)
	}
}
