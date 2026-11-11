// SPDX-License-Identifier: GPL-3.0-or-later

package proxymode

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeLinux is an in-memory desktop: a GSettings store, a kioslaverc store, the
// environment and the tools on PATH. This test has no build tag, so it runs on
// every platform (including the macOS dev machine).
type fakeLinux struct {
	env      map[string]string
	tools    map[string]bool
	gsetting map[string]string // "<schema> <key>" -> GVariant text
	kio      map[string]string // kioslaverc [Proxy Settings]
	noSchema bool
	failSet  string // a "<schema> <key>" or kio key whose write fails
	calls    []string
}

func newFakeGNOME() *fakeLinux {
	return &fakeLinux{
		env:   map[string]string{"XDG_CURRENT_DESKTOP": "ubuntu:GNOME"},
		tools: map[string]bool{"gsettings": true},
		gsetting: map[string]string{
			"org.gnome.system.proxy.http host":      "''",
			"org.gnome.system.proxy.http port":      "8080",
			"org.gnome.system.proxy.https host":     "''",
			"org.gnome.system.proxy.https port":     "0",
			"org.gnome.system.proxy.socks host":     "''",
			"org.gnome.system.proxy.socks port":     "0",
			"org.gnome.system.proxy use-same-proxy": "true",
			"org.gnome.system.proxy ignore-hosts":   "['localhost', '127.0.0.0/8', '::1']",
			"org.gnome.system.proxy mode":           "'none'",
		},
	}
}

func newFakeKDE(version string) *fakeLinux {
	return &fakeLinux{
		env:   map[string]string{"XDG_CURRENT_DESKTOP": "KDE"},
		tools: map[string]bool{"kwriteconfig" + version: true, "kreadconfig" + version: true, "dbus-send": true},
		kio:   map[string]string{"ProxyType": "0"},
	}
}

func (f *fakeLinux) controller() *linuxController {
	return &linuxController{sys: linuxSystem{
		getenv: func(k string) string { return f.env[k] },
		lookPath: func(name string) (string, error) {
			if f.tools[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		run: f.run,
	}}
}

func (f *fakeLinux) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if !f.tools[name] {
		return nil, errors.New(name + ": not found")
	}
	switch {
	case name == "gsettings" && args[0] == "list-keys":
		if f.noSchema {
			return nil, errors.New("No such schema")
		}
		return []byte("mode\n"), nil
	case name == "gsettings" && args[0] == "get":
		v, ok := f.gsetting[args[1]+" "+args[2]]
		if !ok {
			return nil, errors.New("no such key")
		}
		return []byte(v + "\n"), nil
	case name == "gsettings" && args[0] == "set":
		id := args[1] + " " + args[2]
		if id == f.failSet {
			return nil, errors.New("write failed")
		}
		f.gsetting[id] = args[3]
		return nil, nil
	case strings.HasPrefix(name, "kreadconfig"):
		return []byte(f.kio[args[5]] + "\n"), nil
	case strings.HasPrefix(name, "kwriteconfig"):
		if args[1] != "kioslaverc" || args[3] != kdeProxyGroup {
			return nil, errors.New("unexpected file or group")
		}
		if args[5] == f.failSet {
			return nil, errors.New("write failed")
		}
		f.kio[args[5]] = args[6]
		return nil, nil
	case name == "dbus-send":
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}

func TestLinuxDetection(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		tools   []string
		noSchem bool
		want    string
	}{
		{"gnome", map[string]string{"XDG_CURRENT_DESKTOP": "GNOME"}, []string{"gsettings"}, false, linuxDesktopGNOME},
		{"cinnamon", map[string]string{"XDG_CURRENT_DESKTOP": "X-Cinnamon"}, []string{"gsettings"}, false, linuxDesktopGNOME},
		{"desktop session fallback", map[string]string{"DESKTOP_SESSION": "mate"}, []string{"gsettings"}, false, linuxDesktopGNOME},
		{"gnome without gsettings", map[string]string{"XDG_CURRENT_DESKTOP": "GNOME"}, nil, false, ""},
		{"gnome without schema", map[string]string{"XDG_CURRENT_DESKTOP": "GNOME"}, []string{"gsettings"}, true, ""},
		{"kde6", map[string]string{"XDG_CURRENT_DESKTOP": "KDE"}, []string{"kwriteconfig6", "kreadconfig6", "gsettings"}, false, linuxDesktopKDE},
		{"kde5", map[string]string{"XDG_CURRENT_DESKTOP": "KDE"}, []string{"kwriteconfig5", "kreadconfig5"}, false, linuxDesktopKDE},
		{"kde without tools", map[string]string{"XDG_CURRENT_DESKTOP": "KDE"}, []string{"kwriteconfig6", "gsettings"}, false, ""},
		{"unknown desktop", map[string]string{"XDG_CURRENT_DESKTOP": "sway"}, []string{"gsettings", "kwriteconfig6", "kreadconfig6"}, false, ""},
		{"no desktop", nil, []string{"gsettings"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLinux{env: tc.env, tools: map[string]bool{}, noSchema: tc.noSchem}
			for _, tool := range tc.tools {
				f.tools[tool] = true
			}
			c := f.controller()
			if got := c.Supported(); got != (tc.want != "") {
				t.Fatalf("Supported = %v, want %v", got, tc.want != "")
			}
			if c.desktop != tc.want {
				t.Fatalf("desktop = %q, want %q", c.desktop, tc.want)
			}
		})
	}
}

func TestLinuxUnsupportedSetAndSnapshotFail(t *testing.T) {
	f := &fakeLinux{env: map[string]string{"XDG_CURRENT_DESKTOP": "sway"}}
	c := f.controller()
	if _, err := c.Snapshot(); err == nil {
		t.Fatal("Snapshot on an unsupported desktop succeeded")
	}
	if err := c.Set("127.0.0.1", 1080); err == nil {
		t.Fatal("Set on an unsupported desktop succeeded")
	}
	if err := c.Restore(Snapshot{}); err != nil {
		t.Fatalf("Restore of an empty snapshot: %v", err)
	}
}

func TestGNOMESetPointsAtLoopbackModeLast(t *testing.T) {
	f := newFakeGNOME()
	c := f.controller()
	if err := c.Set("127.0.0.1", 46685); err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := map[string]string{
		"org.gnome.system.proxy.http host":  "'127.0.0.1'",
		"org.gnome.system.proxy.http port":  "46685",
		"org.gnome.system.proxy.https host": "'127.0.0.1'",
		"org.gnome.system.proxy.https port": "46685",
		"org.gnome.system.proxy.socks host": "''",
		"org.gnome.system.proxy mode":       "'manual'",
	}
	for k, v := range want {
		if f.gsetting[k] != v {
			t.Errorf("%s = %s, want %s", k, f.gsetting[k], v)
		}
	}
	if !strings.Contains(f.gsetting["org.gnome.system.proxy ignore-hosts"], "'192.168.0.0/16'") {
		t.Errorf("ignore-hosts = %s, want the private ranges", f.gsetting["org.gnome.system.proxy ignore-hosts"])
	}
	if last := f.calls[len(f.calls)-1]; last != "gsettings set org.gnome.system.proxy mode 'manual'" {
		t.Fatalf("last call = %q, want the mode switch", last)
	}
}

func TestGNOMERoundTripRestoresPriorProxies(t *testing.T) {
	priors := map[string]map[string]string{
		"none": {},
		"manual": {
			"org.gnome.system.proxy.http host":      "'10.0.0.1'",
			"org.gnome.system.proxy.http port":      "3128",
			"org.gnome.system.proxy.socks host":     "'10.0.0.2'",
			"org.gnome.system.proxy.socks port":     "1080",
			"org.gnome.system.proxy use-same-proxy": "false",
			"org.gnome.system.proxy ignore-hosts":   "@as []",
			"org.gnome.system.proxy mode":           "'manual'",
		},
		"pac": {
			"org.gnome.system.proxy mode": "'auto'",
		},
	}
	for name, prior := range priors {
		t.Run(name, func(t *testing.T) {
			f := newFakeGNOME()
			for k, v := range prior {
				f.gsetting[k] = v
			}
			before := cloneMap(f.gsetting)
			c := f.controller()

			snap, err := c.Snapshot()
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if snap.Platform != "linux" || snap.Linux == nil || snap.Linux.Desktop != linuxDesktopGNOME {
				t.Fatalf("snapshot = %+v", snap)
			}
			if err := c.Set("127.0.0.1", 46685); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if err := c.Restore(roundTripJSON(t, snap)); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if !reflect.DeepEqual(f.gsetting, before) {
				t.Fatalf("after restore = %v\nwant %v", f.gsetting, before)
			}
		})
	}
}

func TestGNOMESetFailureIsReported(t *testing.T) {
	f := newFakeGNOME()
	f.failSet = "org.gnome.system.proxy mode"
	if err := f.controller().Set("127.0.0.1", 46685); err == nil {
		t.Fatal("Set succeeded despite a failed write")
	}
}

func TestKDESetPointsAtLoopbackAndNotifiesKIO(t *testing.T) {
	f := newFakeKDE("6")
	c := f.controller()
	if err := c.Set("127.0.0.1", 46685); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if f.kio["ProxyType"] != "1" || f.kio["httpProxy"] != "http://127.0.0.1 46685" || f.kio["httpsProxy"] != "http://127.0.0.1 46685" {
		t.Fatalf("kioslaverc = %v", f.kio)
	}
	if f.kio["NoProxyFor"] != kdeProxyBypass || f.kio["ReversedException"] != "false" {
		t.Fatalf("bypass = %q reversed = %q", f.kio["NoProxyFor"], f.kio["ReversedException"])
	}
	n := len(f.calls)
	if !strings.HasPrefix(f.calls[n-1], "dbus-send ") || !strings.Contains(f.calls[n-2], "--key ProxyType 1") {
		t.Fatalf("calls end with %q, %q; want ProxyType then the KIO notification", f.calls[n-2], f.calls[n-1])
	}
}

func TestKDESetWithoutDBusSendStillSucceeds(t *testing.T) {
	f := newFakeKDE("5")
	delete(f.tools, "dbus-send")
	if err := f.controller().Set("::1", 46685); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if f.kio["httpProxy"] != "http://[::1] 46685" {
		t.Fatalf("httpProxy = %q", f.kio["httpProxy"])
	}
}

func TestKDERoundTripRestoresPriorProxies(t *testing.T) {
	priors := map[string]map[string]string{
		"none": {"ProxyType": "0"},
		"manual": {
			"ProxyType": "1", "httpProxy": "http://10.0.0.1 3128", "httpsProxy": "http://10.0.0.1 3128",
			"socksProxy": "socks://10.0.0.2 1080", "NoProxyFor": "intranet.example", "ReversedException": "true",
		},
		"pac": {"ProxyType": "2", "Proxy Config Script": "http://wpad.example/wpad.dat"},
	}
	for name, prior := range priors {
		t.Run(name, func(t *testing.T) {
			f := newFakeKDE("6")
			f.kio = cloneMap(prior)
			c := f.controller()

			snap, err := c.Snapshot()
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if snap.Linux == nil || snap.Linux.Desktop != linuxDesktopKDE {
				t.Fatalf("snapshot = %+v", snap)
			}
			if err := c.Set("127.0.0.1", 46685); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if err := c.Restore(roundTripJSON(t, snap)); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			// Keys absent before come back as empty entries, which KIO reads
			// as unset; every key that had a value gets it back.
			for _, key := range kdeProxyKeys {
				if f.kio[key] != prior[key] {
					t.Errorf("%s = %q, want %q", key, f.kio[key], prior[key])
				}
			}
			if f.kio["Proxy Config Script"] != prior["Proxy Config Script"] {
				t.Errorf("PAC URL changed to %q", f.kio["Proxy Config Script"])
			}
		})
	}
}

func TestLinuxRestoreUsesTheCapturedDesktop(t *testing.T) {
	// A crash-restore on next launch may run in a session whose desktop
	// differs from the one captured; the capture still names its own setting.
	gnome := newFakeGNOME()
	snap, err := gnome.controller().Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	f := newFakeKDE("6")
	f.tools["gsettings"] = true
	f.gsetting = cloneMap(gnome.gsetting)
	f.gsetting["org.gnome.system.proxy mode"] = "'manual'"
	if err := f.controller().Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if f.gsetting["org.gnome.system.proxy mode"] != "'none'" {
		t.Fatalf("mode = %s, want 'none'", f.gsetting["org.gnome.system.proxy mode"])
	}
}

func TestLinuxRestoreRejectsForeignSnapshots(t *testing.T) {
	c := newFakeGNOME().controller()
	if err := c.Restore(Snapshot{Platform: "darwin"}); err == nil {
		t.Fatal("Restore accepted a darwin snapshot")
	}
	if err := c.Restore(Snapshot{Platform: "linux", Linux: &LinuxProxyState{Desktop: "lxqt"}}); err == nil {
		t.Fatal("Restore accepted an unknown desktop")
	}
}

func TestSnapshotJSONStaysBackwardCompatible(t *testing.T) {
	// Snapshots persisted before the Linux field existed still decode, and
	// non-Linux snapshots encode without it.
	var old Snapshot
	if err := json.Unmarshal([]byte(`{"platform":"windows","windows":{"proxy_enable":true}}`), &old); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if old.Linux != nil || old.Windows == nil {
		t.Fatalf("decoded = %+v", old)
	}
	out, err := json.Marshal(Snapshot{Platform: "darwin"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(out), "linux") {
		t.Fatalf("darwin snapshot encodes a linux field: %s", out)
	}
}

func TestGVariantStringEscapes(t *testing.T) {
	if got := gvariantString(`a'b\c`); got != `'a\'b\\c'` {
		t.Fatalf("gvariantString = %s", got)
	}
}

func roundTripJSON(t *testing.T, snap Snapshot) Snapshot {
	t.Helper()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out Snapshot
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
