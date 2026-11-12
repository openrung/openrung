package proxymode

import (
	"fmt"
	"net"
	"strconv"
)

// defaultProxyBypass keeps loopback, link-local, and private LAN destinations
// off the relay: a user's router page, NAS, or localhost dev server should stay
// direct, and — critically — traffic to 127.0.0.1 must not be routed back into
// the loopback proxy itself. Mirrors the bypass list common Windows proxy
// clients apply. "<local>" matches any hostname without a dot.
const defaultProxyBypass = "localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*;<local>"

// winProxyBackend is the OS-facing surface of the Windows controller: WinInet
// proxy setting reads/writes plus the settings-changed notification. It is an
// interface so the controller logic is unit-testable without touching real settings;
// the concrete wininetBackend lives in windows.go, built only on Windows.
type winProxyBackend interface {
	read() (WindowsProxyState, error)
	write(WindowsProxyState) error
	notify() error
}

// windowsController drives the per-user WinInet proxy. Unlike macOS there is a
// single global proxy setting rather than one per network service, so the
// snapshot carries a single WindowsProxyState.
type windowsController struct {
	backend winProxyBackend
}

func (c *windowsController) Supported() bool { return true }

func (c *windowsController) Snapshot() (Snapshot, error) {
	state, err := c.backend.read()
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Platform: "windows", Windows: &state}, nil
}

func (c *windowsController) Set(host string, port int) error {
	state := WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   net.JoinHostPort(host, strconv.Itoa(port)),
		ProxyOverride: defaultProxyBypass,
		// Clear any PAC URL and switch off automatic detection: an auto-config
		// script, configured or discovered, takes precedence over the static
		// proxy we just set and would silently defeat it. The prior values are
		// captured in the snapshot and restored on disconnect.
		AutoConfigURL: "",
		AutoDetect:    new(bool),
	}
	if err := c.backend.write(state); err != nil {
		return fmt.Errorf("set windows proxy: %w", err)
	}
	if err := c.backend.notify(); err != nil {
		return fmt.Errorf("notify windows proxy change: %w", err)
	}
	return nil
}

func (c *windowsController) Restore(snap Snapshot) error {
	if snap.Platform != "" && snap.Platform != "windows" {
		return fmt.Errorf("cannot restore %q proxy snapshot on windows", snap.Platform)
	}
	// A nil capture (e.g. an empty snapshot from a crash before Snapshot ran)
	// restores to "proxy disabled", which safely undoes our Set.
	var state WindowsProxyState
	if snap.Windows != nil {
		state = *snap.Windows
	}
	// Without a captured auto-detect setting, keep the current one.
	if state.AutoDetect == nil {
		current, err := c.backend.read()
		if err != nil {
			return fmt.Errorf("restore windows proxy: %w", err)
		}
		state.AutoDetect = current.AutoDetect
	}
	if err := c.backend.write(state); err != nil {
		return fmt.Errorf("restore windows proxy: %w", err)
	}
	if err := c.backend.notify(); err != nil {
		return fmt.Errorf("notify windows proxy change: %w", err)
	}
	return nil
}
