package proxymode

import (
	"fmt"
	"net"
	"strconv"
)

// requiredProxyBypass prevents the local mixed proxy from recursively proxying
// loopback requests. It stays active even when the split-tunnel LAN preset is
// off. "<local>" and private ranges live in lanProxyBypass so the user's LAN
// toggle is not silently overridden by WinInet before traffic reaches sing-box.
const requiredProxyBypass = "localhost;127.*;[::1]"

const lanProxyBypass = "10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*;<local>"

const defaultProxyBypass = requiredProxyBypass + ";" + lanProxyBypass

// winProxyBackend is the OS-facing surface of the Windows controller: the
// WinInet registry reads/writes plus the settings-changed notification. It is an
// interface so the controller logic is unit-testable without a real registry;
// the concrete registryBackend lives in windows.go, built only on Windows.
type winProxyBackend interface {
	read() (WindowsProxyState, error)
	write(WindowsProxyState) error
	notify() error
}

// windowsController drives the per-user WinInet proxy under
// HKCU\...\Internet Settings. Unlike macOS there is a single global proxy
// setting rather than one per network service, so the snapshot carries a single
// WindowsProxyState.
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
	return c.SetWithOptions(host, port, SetOptions{BypassLAN: true})
}

func (c *windowsController) SetWithOptions(host string, port int, options SetOptions) error {
	proxyBypass := requiredProxyBypass
	if options.BypassLAN {
		proxyBypass = defaultProxyBypass
	}
	state := WindowsProxyState{
		ProxyEnable:   true,
		ProxyServer:   net.JoinHostPort(host, strconv.Itoa(port)),
		ProxyOverride: proxyBypass,
		// Clear any PAC URL: a stale auto-config script takes precedence over the
		// static proxy we just set and would silently defeat it. The prior value
		// is captured in the snapshot and restored on disconnect.
		AutoConfigURL: "",
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
	if err := c.backend.write(state); err != nil {
		return fmt.Errorf("restore windows proxy: %w", err)
	}
	if err := c.backend.notify(); err != nil {
		return fmt.Errorf("notify windows proxy change: %w", err)
	}
	return nil
}
