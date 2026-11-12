// SPDX-License-Identifier: GPL-3.0-or-later

package proxymode

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

// Linux has no OS-wide proxy setting, only per-desktop ones that browsers and
// other desktop apps read: GSettings (org.gnome.system.proxy) on GNOME and the
// GTK desktops that share its schema, and KIO's kioslaverc on KDE Plasma. The
// controller drives whichever the session's desktop uses through the desktop's
// own command-line tools, so it needs no D-Bus or dconf bindings.
//
// On Plasma it also writes GSettings when that schema is installed: Firefox
// and GTK apps read it on every desktop and never read kioslaverc.

const (
	linuxDesktopGNOME = "gnome"
	linuxDesktopKDE   = "kde"
)

// gnomeProxyKeys are the org.gnome.system.proxy keys Set may change, in the
// order Restore writes them back. "mode" comes last so apps switch from one
// complete configuration to the other rather than seeing a half-written one.
var gnomeProxyKeys = []gnomeKey{
	{"org.gnome.system.proxy.http", "host"},
	{"org.gnome.system.proxy.http", "port"},
	{"org.gnome.system.proxy.https", "host"},
	{"org.gnome.system.proxy.https", "port"},
	{"org.gnome.system.proxy.socks", "host"},
	{"org.gnome.system.proxy.socks", "port"},
	{"org.gnome.system.proxy", "use-same-proxy"},
	{"org.gnome.system.proxy", "ignore-hosts"},
	{"org.gnome.system.proxy", "mode"},
}

type gnomeKey struct{ schema, key string }

func (k gnomeKey) id() string { return k.schema + " " + k.key }

// gnomeProxyBypass mirrors defaultProxyBypass in GNOME's ignore-hosts syntax.
var gnomeProxyBypass = []string{
	"localhost", "127.0.0.0/8", "::1", "10.0.0.0/8", "172.16.0.0/12",
	"192.168.0.0/16", "169.254.0.0/16", "fe80::/10", "*.local",
}

const kdeProxyGroup = "Proxy Settings"

// kdeProxyKeys are the kioslaverc [Proxy Settings] keys Set may change, in the
// order Restore writes them back; ProxyType, the switch, comes last.
var kdeProxyKeys = []string{
	"httpProxy", "httpsProxy", "socksProxy", "NoProxyFor", "ReversedException", "ProxyType",
}

// kdeProxyBypass mirrors defaultProxyBypass in KIO's NoProxyFor syntax.
const kdeProxyBypass = "localhost,127.0.0.0/8,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fe80::/10,.local"

// gnomeDesktops are the desktops on which both Chromium and Firefox read
// org.gnome.system.proxy. Desktops that list GNOME as a fallback entry
// ("ubuntu:GNOME", "Budgie:GNOME") match through it. XFCE is deliberately
// absent: Chromium reads only the proxy environment variables there, so
// writing GSettings would report a system proxy that Chromium ignores.
var gnomeDesktops = map[string]bool{
	"gnome": true, "unity": true, "x-cinnamon": true, "pantheon": true, "mate": true,
}

// linuxSystem is the OS-facing surface of the Linux controller, injectable so
// tests need no desktop session.
type linuxSystem struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      func(name string, args ...string) ([]byte, error)
}

// linuxController drives the session desktop's proxy setting. Detection runs
// once, on first use; anything other than a GNOME-family or KDE session with
// its tools installed is unsupported, leaving the manual-proxy fallback.
type linuxController struct {
	sys linuxSystem

	once      sync.Once
	desktop   string // linuxDesktopGNOME, linuxDesktopKDE, or "" if unsupported
	gsettings bool   // org.gnome.system.proxy is driven: always on GNOME, on KDE when installed
	kwrite    string // kwriteconfig6 or kwriteconfig5
	kread     string // the matching kreadconfig
}

func (c *linuxController) detect() {
	c.once.Do(func() {
		desktops := c.sys.getenv("XDG_CURRENT_DESKTOP")
		if desktops == "" {
			desktops = c.sys.getenv("DESKTOP_SESSION")
		}
		for _, entry := range strings.Split(desktops, ":") {
			entry = strings.ToLower(strings.TrimSpace(entry))
			switch {
			case entry == "kde" || strings.HasPrefix(entry, "plasma"):
				if c.detectKDE() {
					return
				}
			case gnomeDesktops[entry]:
				if c.detectGNOME() {
					return
				}
			}
		}
	})
}

func (c *linuxController) detectKDE() bool {
	write, read, ok := c.kdeTools()
	if ok {
		c.desktop, c.kwrite, c.kread = linuxDesktopKDE, write, read
		c.gsettings = c.gsettingsUsable()
	}
	return ok
}

// kdeTools finds the KConfig command-line tools, preferring Plasma 6's.
func (c *linuxController) kdeTools() (write, read string, ok bool) {
	for _, v := range []string{"6", "5"} {
		_, werr := c.sys.lookPath("kwriteconfig" + v)
		_, rerr := c.sys.lookPath("kreadconfig" + v)
		if werr == nil && rerr == nil {
			return "kwriteconfig" + v, "kreadconfig" + v, true
		}
	}
	return "", "", false
}

func (c *linuxController) detectGNOME() bool {
	if !c.gsettingsUsable() {
		return false
	}
	c.desktop, c.gsettings = linuxDesktopGNOME, true
	return true
}

// gsettingsUsable reports whether gsettings and the proxy schema are
// installed. The schema ships with gsettings-desktop-schemas; without it
// every get and set fails.
func (c *linuxController) gsettingsUsable() bool {
	if _, err := c.sys.lookPath("gsettings"); err != nil {
		return false
	}
	_, err := c.sys.run("gsettings", "list-keys", "org.gnome.system.proxy")
	return err == nil
}

func (c *linuxController) Supported() bool {
	c.detect()
	return c.desktop != ""
}

func (c *linuxController) Snapshot() (Snapshot, error) {
	c.detect()
	if c.desktop == "" {
		return Snapshot{}, errors.New("no supported desktop proxy setting in this session")
	}
	// The two settings' keys cannot collide (GSettings ones carry their
	// schema), so a KDE capture holds both in one map.
	state := LinuxProxyState{Desktop: c.desktop, Settings: map[string]string{}}
	if c.desktop == linuxDesktopKDE {
		for _, key := range kdeProxyKeys {
			out, err := c.sys.run(c.kread, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", key)
			if err != nil {
				return Snapshot{}, fmt.Errorf("read kioslaverc %s: %w", key, err)
			}
			state.Settings[key] = strings.TrimRight(string(out), "\r\n")
		}
	}
	if c.gsettings {
		for _, k := range gnomeProxyKeys {
			out, err := c.sys.run("gsettings", "get", k.schema, k.key)
			if err != nil {
				return Snapshot{}, fmt.Errorf("read %s: %w", k.id(), err)
			}
			// gsettings prints the value in GVariant text form, which
			// "gsettings set" parses back, so it restores verbatim.
			state.Settings[k.id()] = strings.TrimSpace(string(out))
		}
	}
	return Snapshot{Platform: "linux", Linux: &state}, nil
}

func (c *linuxController) Set(host string, port int) error {
	c.detect()
	if c.desktop == "" {
		return errors.New("no supported desktop proxy setting in this session")
	}
	portStr := strconv.Itoa(port)
	if c.desktop == linuxDesktopKDE {
		// KIO stores a manual proxy as "scheme://host port".
		endpoint := "http://" + bracketIPv6(host) + " " + portStr
		if err := c.writeKDE(c.kwrite, c.kread, map[string]string{
			"httpProxy":         endpoint,
			"httpsProxy":        endpoint,
			"socksProxy":        "",
			"NoProxyFor":        kdeProxyBypass,
			"ReversedException": "false",
			"ProxyType":         "1", // manual
		}); err != nil {
			return err
		}
	}
	if !c.gsettings {
		return nil
	}
	return c.writeGNOME(map[string]string{
		"org.gnome.system.proxy.http host":  gvariantString(host),
		"org.gnome.system.proxy.http port":  portStr,
		"org.gnome.system.proxy.https host": gvariantString(host),
		"org.gnome.system.proxy.https port": portStr,
		// Clear SOCKS so a prior upstream SOCKS proxy is not used
		// alongside ours; it is captured and restored on disconnect.
		"org.gnome.system.proxy.socks host":     gvariantString(""),
		"org.gnome.system.proxy.socks port":     "0",
		"org.gnome.system.proxy use-same-proxy": "false",
		"org.gnome.system.proxy ignore-hosts":   gvariantStringArray(gnomeProxyBypass),
		"org.gnome.system.proxy mode":           gvariantString("manual"),
	})
}

func (c *linuxController) Restore(snap Snapshot) error {
	if snap.Platform != "" && snap.Platform != "linux" {
		return fmt.Errorf("cannot restore %q proxy snapshot on linux", snap.Platform)
	}
	// No capture means Set never ran against this snapshot (Snapshot always
	// fills it first), so there is nothing of ours to undo.
	if snap.Linux == nil {
		return nil
	}
	// Restore through the desktop the capture came from, not the one detected
	// now: a crash-restore on next launch may run in a different session.
	switch snap.Linux.Desktop {
	case linuxDesktopGNOME:
		return c.writeGNOME(snap.Linux.Settings)
	case linuxDesktopKDE:
		// Attempt both settings even if one fails, so a missing tool never
		// strands the other pointing at the tunnel. A capture without
		// GSettings keys makes writeGNOME a no-op.
		var kdeErr error
		if write, read, ok := c.kdeTools(); ok {
			kdeErr = c.writeKDE(write, read, snap.Linux.Settings)
		} else {
			kdeErr = errors.New("the KConfig tools are not available to restore the KDE proxy setting")
		}
		return errors.Join(kdeErr, c.writeGNOME(snap.Linux.Settings))
	default:
		return fmt.Errorf("cannot restore linux proxy snapshot for desktop %q", snap.Linux.Desktop)
	}
}

// writeGNOME sets every key in values, in gnomeProxyKeys order, then confirms
// each one took effect. Keys absent from values are left alone.
//
// The exit status of "gsettings set" does not prove the write: when the change
// cannot be committed gsettings only logs a warning and still exits 0. A
// separate process therefore reads every key back, and a value that did not
// stick is an error, so the caller never drops a snapshot whose restore was
// lost. Values must be in the text form gsettings prints.
func (c *linuxController) writeGNOME(values map[string]string) error {
	for _, k := range gnomeProxyKeys {
		value, ok := values[k.id()]
		if !ok {
			continue
		}
		if _, err := c.sys.run("gsettings", "set", k.schema, k.key, value); err != nil {
			return fmt.Errorf("set %s: %w", k.id(), err)
		}
	}
	for _, k := range gnomeProxyKeys {
		want, ok := values[k.id()]
		if !ok {
			continue
		}
		out, err := c.sys.run("gsettings", "get", k.schema, k.key)
		if err != nil {
			return fmt.Errorf("confirm %s: %w", k.id(), err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			return fmt.Errorf("set %s: wrote %s but it reads back as %s", k.id(), want, got)
		}
	}
	return nil
}

// writeKDE sets every key in values, in kdeProxyKeys order, asks running KIO
// workers to reread their configuration, then confirms each key took effect.
// Keys absent from values are left alone; an empty value writes an empty
// entry, which KIO reads as unset.
//
// As with GSettings, the exit status does not prove the write: kwriteconfig
// does not check that the file was saved. kread therefore reads every key back
// in a separate process, and a value that did not stick is an error.
func (c *linuxController) writeKDE(kwrite, kread string, values map[string]string) error {
	for _, key := range kdeProxyKeys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if _, err := c.sys.run(kwrite, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", key, value); err != nil {
			return fmt.Errorf("write kioslaverc %s: %w", key, err)
		}
	}
	// Best effort: apps also reread kioslaverc when they next start, so a
	// missing dbus-send or session bus only delays the change for running ones.
	if _, err := c.sys.lookPath("dbus-send"); err == nil {
		_, _ = c.sys.run("dbus-send", "--type=signal", "/KIO/Scheduler",
			"org.kde.KIO.Scheduler.reparseSlaveConfiguration", "string:")
	}
	for _, key := range kdeProxyKeys {
		want, ok := values[key]
		if !ok {
			continue
		}
		out, err := c.sys.run(kread, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", key)
		if err != nil {
			return fmt.Errorf("confirm kioslaverc %s: %w", key, err)
		}
		if got := strings.TrimRight(string(out), "\r\n"); got != want {
			return fmt.Errorf("write kioslaverc %s: wrote %q but it reads back as %q", key, want, got)
		}
	}
	return nil
}

// gvariantString renders s as a GVariant text-format string literal.
func gvariantString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func gvariantStringArray(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = gvariantString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func bracketIPv6(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}
