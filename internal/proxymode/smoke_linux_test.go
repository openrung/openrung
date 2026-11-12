// SPDX-License-Identifier: GPL-3.0-or-later

package proxymode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLinuxRealDesktopTools drives the real gsettings, kwriteconfig and
// kreadconfig binaries, which the fake desktop in linux_controller_test.go
// cannot stand in for: it stores whatever it is handed, while the tools parse
// their arguments and exit 0 even when a write is lost. Every check therefore
// reads the setting back in a separate process.
//
// It rewrites the session's proxy setting, so it only runs when
// OPENRUNG_PROXYMODE_SMOKE names the desktop to act as ("gnome" or "kde"). CI
// runs it under dbus-run-session with a throwaway XDG_CONFIG_HOME.
func TestLinuxRealDesktopTools(t *testing.T) {
	desktop := os.Getenv("OPENRUNG_PROXYMODE_SMOKE")
	switch desktop {
	case "":
		t.Skip("set OPENRUNG_PROXYMODE_SMOKE=gnome or kde in a throwaway session to run")
	case linuxDesktopGNOME:
		t.Setenv("XDG_CURRENT_DESKTOP", "ubuntu:GNOME")
	case linuxDesktopKDE:
		t.Setenv("XDG_CURRENT_DESKTOP", "KDE")
	default:
		t.Fatalf("OPENRUNG_PROXYMODE_SMOKE = %q, want gnome or kde", desktop)
	}

	c := New().(*linuxController)
	if !c.Supported() || c.desktop != desktop {
		t.Fatalf("detected desktop %q, want %q (are its tools installed?)", c.desktop, desktop)
	}
	// Both runs cover GSettings: it is the GNOME setting, and on KDE the one
	// Firefox reads.
	if !c.gsettings {
		t.Fatal("gsettings or the org.gnome.system.proxy schema is missing")
	}

	// A prior upstream proxy with an empty bypass list: the values a restore
	// is most likely to get wrong (an empty array prints as "@as []").
	for _, kv := range [][3]string{
		{"org.gnome.system.proxy.http", "host", "'upstream.example'"},
		{"org.gnome.system.proxy.http", "port", "3128"},
		{"org.gnome.system.proxy.socks", "host", "'socks.example'"},
		{"org.gnome.system.proxy.socks", "port", "1080"},
		{"org.gnome.system.proxy", "ignore-hosts", "[]"},
		{"org.gnome.system.proxy", "mode", "'manual'"},
	} {
		smokeRun(t, "gsettings", "set", kv[0], kv[1], kv[2])
	}
	if desktop == linuxDesktopKDE {
		for _, kv := range [][2]string{
			{"httpProxy", "http://upstream.example 3128"},
			{"NoProxyFor", "intranet.example"},
			{"ProxyType", "1"},
		} {
			smokeRun(t, c.kwrite, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", kv[0], kv[1])
		}
	}

	read := func() map[string]string {
		state := map[string]string{}
		for _, k := range gnomeProxyKeys {
			state[k.id()] = smokeRun(t, "gsettings", "get", k.schema, k.key)
		}
		if desktop == linuxDesktopKDE {
			for _, key := range kdeProxyKeys {
				state[key] = smokeRun(t, c.kread, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", key)
			}
		}
		return state
	}
	before := read()
	if before["org.gnome.system.proxy.http host"] != "'upstream.example'" {
		t.Fatalf("seeded GSettings did not persist across processes: %v", before)
	}

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Restore from the persisted form, as a crash recovery would.
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	var persisted Snapshot
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	t.Cleanup(func() { _ = c.Restore(persisted) })

	if err := c.Set("127.0.0.1", 46685); err != nil {
		t.Fatalf("Set: %v", err)
	}
	set := read()
	for key, want := range map[string]string{
		"org.gnome.system.proxy mode":       "'manual'",
		"org.gnome.system.proxy.http host":  "'127.0.0.1'",
		"org.gnome.system.proxy.http port":  "46685",
		"org.gnome.system.proxy.https host": "'127.0.0.1'",
		"org.gnome.system.proxy.https port": "46685",
		"org.gnome.system.proxy.socks host": "''",
	} {
		if set[key] != want {
			t.Errorf("after Set, %s = %s, want %s", key, set[key], want)
		}
	}
	if got := set["org.gnome.system.proxy ignore-hosts"]; !strings.Contains(got, "'*.local'") {
		t.Errorf("after Set, ignore-hosts = %s, want the bypass list", got)
	}
	if desktop == linuxDesktopKDE {
		// Browsers parse the file itself, so check what landed on disk.
		dir, err := os.UserConfigDir()
		if err != nil {
			t.Fatalf("config dir: %v", err)
		}
		file, err := os.ReadFile(filepath.Join(dir, "kioslaverc"))
		if err != nil {
			t.Fatalf("read kioslaverc: %v", err)
		}
		lines := strings.Split(string(file), "\n")
		for _, want := range []string{
			"[" + kdeProxyGroup + "]",
			"ProxyType=1",
			"httpProxy=http://127.0.0.1 46685",
			"httpsProxy=http://127.0.0.1 46685",
			"NoProxyFor=" + kdeProxyBypass,
		} {
			found := false
			for _, line := range lines {
				found = found || strings.TrimSpace(line) == want
			}
			if !found {
				t.Errorf("kioslaverc lacks the line %q:\n%s", want, file)
			}
		}
	}

	if err := c.Restore(persisted); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if after := read(); !reflect.DeepEqual(after, before) {
		t.Fatalf("after Restore = %v\nwant %v", after, before)
	}
}

// smokeRun runs a real desktop tool and returns its trimmed output.
func smokeRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimRight(string(out), "\r\n")
}
