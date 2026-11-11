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

// gnomeDesktops are XDG_CURRENT_DESKTOP entries whose apps read
// org.gnome.system.proxy (the same set Chromium treats as GSettings desktops).
var gnomeDesktops = map[string]bool{
	"gnome": true, "unity": true, "ubuntu": true, "cinnamon": true, "x-cinnamon": true,
	"pantheon": true, "budgie": true, "budgie-desktop": true, "mate": true, "xfce": true,
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

	once    sync.Once
	desktop string // linuxDesktopGNOME, linuxDesktopKDE, or "" if unsupported
	kwrite  string // kwriteconfig6 or kwriteconfig5
	kread   string // the matching kreadconfig
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
	if _, err := c.sys.lookPath("gsettings"); err != nil {
		return false
	}
	// The schema ships with gsettings-desktop-schemas; without it every get
	// and set fails, so treat its absence as unsupported.
	if _, err := c.sys.run("gsettings", "list-keys", "org.gnome.system.proxy"); err != nil {
		return false
	}
	c.desktop = linuxDesktopGNOME
	return true
}

func (c *linuxController) Supported() bool {
	c.detect()
	return c.desktop != ""
}

func (c *linuxController) Snapshot() (Snapshot, error) {
	c.detect()
	state := LinuxProxyState{Desktop: c.desktop, Settings: map[string]string{}}
	switch c.desktop {
	case linuxDesktopGNOME:
		for _, k := range gnomeProxyKeys {
			out, err := c.sys.run("gsettings", "get", k.schema, k.key)
			if err != nil {
				return Snapshot{}, fmt.Errorf("read %s: %w", k.id(), err)
			}
			// gsettings prints the value in GVariant text form, which
			// "gsettings set" parses back, so it restores verbatim.
			state.Settings[k.id()] = strings.TrimSpace(string(out))
		}
	case linuxDesktopKDE:
		for _, key := range kdeProxyKeys {
			out, err := c.sys.run(c.kread, "--file", "kioslaverc", "--group", kdeProxyGroup, "--key", key)
			if err != nil {
				return Snapshot{}, fmt.Errorf("read kioslaverc %s: %w", key, err)
			}
			state.Settings[key] = strings.TrimRight(string(out), "\r\n")
		}
	default:
		return Snapshot{}, errors.New("no supported desktop proxy setting in this session")
	}
	return Snapshot{Platform: "linux", Linux: &state}, nil
}

func (c *linuxController) Set(host string, port int) error {
	c.detect()
	switch c.desktop {
	case linuxDesktopGNOME:
		portStr := strconv.Itoa(port)
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
	case linuxDesktopKDE:
		// KIO stores a manual proxy as "scheme://host port".
		endpoint := "http://" + bracketIPv6(host) + " " + strconv.Itoa(port)
		return c.writeKDE(c.kwrite, map[string]string{
			"httpProxy":         endpoint,
			"httpsProxy":        endpoint,
			"socksProxy":        "",
			"NoProxyFor":        kdeProxyBypass,
			"ReversedException": "false",
			"ProxyType":         "1", // manual
		})
	default:
		return errors.New("no supported desktop proxy setting in this session")
	}
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
		write, _, ok := c.kdeTools()
		if !ok {
			return errors.New("kwriteconfig is not available to restore the KDE proxy setting")
		}
		return c.writeKDE(write, snap.Linux.Settings)
	default:
		return fmt.Errorf("cannot restore linux proxy snapshot for desktop %q", snap.Linux.Desktop)
	}
}

// writeGNOME sets every key in values, in gnomeProxyKeys order. Keys absent
// from values are left alone.
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
	return nil
}

// writeKDE sets every key in values, in kdeProxyKeys order, then asks running
// KIO workers to reread their configuration. Keys absent from values are left
// alone; an empty value writes an empty entry, which KIO reads as unset.
func (c *linuxController) writeKDE(kwrite string, values map[string]string) error {
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
