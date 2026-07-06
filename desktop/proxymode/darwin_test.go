//go:build darwin

package proxymode

import (
	"fmt"
	"strings"
	"testing"
)

// fakeNetworksetup scripts canned outputs and records the mutating calls.
type fakeNetworksetup struct {
	calls [][]string
}

func (f *fakeNetworksetup) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "-listallnetworkservices":
		return []byte("An asterisk (*) denotes that a network service is disabled.\nWi-Fi\nEthernet\n*Old Adapter\n"), nil
	case "-getwebproxy":
		return []byte("Enabled: Yes\nServer: 10.0.0.1\nPort: 3128\nAuthenticated Proxy Enabled: 0\n"), nil
	case "-getsecurewebproxy":
		return []byte("Enabled: No\nServer:\nPort: 0\nAuthenticated Proxy Enabled: 0\n"), nil
	}
	return nil, nil
}

func (f *fakeNetworksetup) mutations() []string {
	var out []string
	for _, call := range f.calls {
		if strings.HasPrefix(call[0], "-set") {
			out = append(out, strings.Join(call, " "))
		}
	}
	return out
}

func TestSnapshotSkipsHeaderAndDisabledServices(t *testing.T) {
	fake := &fakeNetworksetup{}
	c := &darwinController{run: fake.run}

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Platform != "darwin" {
		t.Fatalf("platform = %q", snap.Platform)
	}
	// Wi-Fi and Ethernet only; the header and "*Old Adapter" are skipped.
	if len(snap.Services) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(snap.Services), snap.Services)
	}
	wifi := snap.Services[0]
	if wifi.Name != "Wi-Fi" || !wifi.WebEnabled || wifi.WebHost != "10.0.0.1" || wifi.WebPort != 3128 {
		t.Fatalf("unexpected Wi-Fi web proxy: %+v", wifi)
	}
	if wifi.SecureEnabled {
		t.Fatalf("secure proxy should be disabled: %+v", wifi)
	}
}

func TestSetAppliesToEachEnabledService(t *testing.T) {
	fake := &fakeNetworksetup{}
	c := &darwinController{run: fake.run}

	if err := c.Set("127.0.0.1", 7890); err != nil {
		t.Fatalf("Set: %v", err)
	}
	muts := fake.mutations()
	want := []string{
		"-setwebproxy Wi-Fi 127.0.0.1 7890",
		"-setsecurewebproxy Wi-Fi 127.0.0.1 7890",
		"-setwebproxy Ethernet 127.0.0.1 7890",
		"-setsecurewebproxy Ethernet 127.0.0.1 7890",
	}
	if strings.Join(muts, "|") != strings.Join(want, "|") {
		t.Fatalf("Set mutations:\n got %v\nwant %v", muts, want)
	}
}

func TestRestoreReappliesEnabledAndDisablesOff(t *testing.T) {
	fake := &fakeNetworksetup{}
	c := &darwinController{run: fake.run}

	snap := Snapshot{
		Platform: "darwin",
		Services: []ServiceProxyState{
			{Name: "Wi-Fi", WebEnabled: true, WebHost: "10.0.0.1", WebPort: 3128, SecureEnabled: false},
		},
	}
	if err := c.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	muts := fake.mutations()
	// Enabled web proxy is re-set; the disabled secure proxy is turned off.
	assertContains(t, muts, "-setwebproxy Wi-Fi 10.0.0.1 3128")
	assertContains(t, muts, "-setsecurewebproxystate Wi-Fi off")
}

func TestRestoreRejectsForeignPlatform(t *testing.T) {
	c := &darwinController{run: (&fakeNetworksetup{}).run}
	err := c.Restore(Snapshot{Platform: "windows"})
	if err == nil {
		t.Fatal("expected refusal to restore a non-darwin snapshot")
	}
}

func assertContains(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, h := range haystack {
		if h == needle {
			return
		}
	}
	t.Fatalf("expected mutation %q in %v", needle, fmt.Sprint(haystack))
}
