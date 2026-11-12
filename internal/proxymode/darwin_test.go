//go:build darwin

package proxymode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeNetworksetup scripts canned outputs and records the mutating calls.
type fakeNetworksetup struct {
	calls [][]string
	// autoProxy is each service's -getautoproxyurl output; unset services
	// report no PAC URL.
	autoProxy map[string]string
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
	case "-getautoproxyurl":
		if out, ok := f.autoProxy[args[1]]; ok {
			return []byte(out), nil
		}
		return []byte("URL: (null)\nEnabled: No\n"), nil
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
		"-setautoproxystate Wi-Fi off",
		"-setwebproxy Ethernet 127.0.0.1 7890",
		"-setsecurewebproxy Ethernet 127.0.0.1 7890",
		"-setautoproxystate Ethernet off",
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

func TestAutoProxyIsCapturedDisabledAndRestored(t *testing.T) {
	// A PAC URL takes precedence over the manual proxies Set writes, so Set
	// turns it off; the snapshot keeps it so Restore turns it back on.
	fake := &fakeNetworksetup{autoProxy: map[string]string{
		"Wi-Fi": "URL: http://wpad.example/proxy.pac\nEnabled: Yes\n",
	}}
	c := &darwinController{run: fake.run}

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	wifi, ethernet := snap.Services[0].AutoProxy, snap.Services[1].AutoProxy
	if wifi == nil || *wifi != (AutoProxyState{Enabled: true, URL: "http://wpad.example/proxy.pac"}) {
		t.Fatalf("Wi-Fi auto proxy = %+v", wifi)
	}
	if ethernet == nil || *ethernet != (AutoProxyState{}) {
		t.Fatalf("Ethernet auto proxy = %+v, want captured as off with no URL", ethernet)
	}

	if err := c.Set("127.0.0.1", 7890); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertContains(t, fake.mutations(), "-setautoproxystate Wi-Fi off")

	fake.calls = nil
	if err := c.Restore(roundTripJSON(t, snap)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	muts := fake.mutations()
	assertContains(t, muts, "-setautoproxyurl Wi-Fi http://wpad.example/proxy.pac")
	for _, m := range muts {
		if strings.Contains(m, "autoproxy") && strings.Contains(m, "Ethernet") {
			t.Fatalf("Restore touched Ethernet's PAC setting, which was off: %q", m)
		}
	}
}

func TestRestoreOfSnapshotWithoutAutoProxyLeavesItAlone(t *testing.T) {
	// A snapshot persisted by an earlier version has no auto_proxy field: that
	// version's Set never changed the PAC setting, so Restore must not either.
	var snap Snapshot
	persisted := `{"platform":"darwin","services":[{"name":"Wi-Fi","web_enabled":false,"secure_enabled":false}]}`
	if err := json.Unmarshal([]byte(persisted), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fake := &fakeNetworksetup{}
	if err := (&darwinController{run: fake.run}).Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, m := range fake.mutations() {
		if strings.Contains(m, "autoproxy") {
			t.Fatalf("Restore of an old snapshot changed the PAC setting: %q", m)
		}
	}
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
