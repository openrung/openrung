//go:build windows

package proxymode

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// New returns the Windows proxy controller. It drives the current user's WinInet
// proxy values under HKCU\...\Internet Settings and signals WinInet so already
// running apps (Edge, Chrome, and anything else on WinInet) pick up the change
// live rather than only on restart.
func New() Controller {
	return &windowsController{backend: registryBackend{}}
}

const internetSettingsPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// registryBackend is the real winProxyBackend: it reads and writes the WinInet
// proxy values in the current user's registry and notifies WinInet of changes.
type registryBackend struct{}

func (registryBackend) read() (WindowsProxyState, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsPath, registry.QUERY_VALUE)
	if err != nil {
		return WindowsProxyState{}, fmt.Errorf("open internet settings: %w", err)
	}
	defer key.Close()

	enable, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return WindowsProxyState{}, fmt.Errorf("read ProxyEnable: %w", err)
	}
	return WindowsProxyState{
		ProxyEnable:   enable != 0,
		ProxyServer:   readString(key, "ProxyServer"),
		ProxyOverride: readString(key, "ProxyOverride"),
		AutoConfigURL: readString(key, "AutoConfigURL"),
	}, nil
}

// readString returns a string value, treating a missing value as empty so a
// snapshot can capture "unset" without a special case.
func readString(key registry.Key, name string) string {
	v, _, err := key.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

func (registryBackend) write(state WindowsProxyState) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open internet settings: %w", err)
	}
	defer key.Close()

	var enable uint32
	if state.ProxyEnable {
		enable = 1
	}
	if err := key.SetDWordValue("ProxyEnable", enable); err != nil {
		return fmt.Errorf("write ProxyEnable: %w", err)
	}
	if err := writeString(key, "ProxyServer", state.ProxyServer); err != nil {
		return err
	}
	if err := writeString(key, "ProxyOverride", state.ProxyOverride); err != nil {
		return err
	}
	if err := writeString(key, "AutoConfigURL", state.AutoConfigURL); err != nil {
		return err
	}
	return nil
}

// writeString sets a string value, or deletes it when empty so restoring an
// absent value (e.g. no PAC URL) leaves the registry clean rather than holding a
// stray empty string.
func writeString(key registry.Key, name, value string) error {
	if value == "" {
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("delete %s: %w", name, err)
		}
		return nil
	}
	if err := key.SetStringValue(name, value); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

var (
	wininet                = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOptionW = wininet.NewProc("InternetSetOptionW")
)

// WinInet options (wininet.h): announce that the settings changed, then force a
// refresh so running processes reread them.
const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

func (registryBackend) notify() error {
	if err := internetSetOption(internetOptionSettingsChanged); err != nil {
		return err
	}
	return internetSetOption(internetOptionRefresh)
}

// internetSetOption calls InternetSetOptionW(NULL, option, NULL, 0): both
// notifications take no buffer. A zero return is failure, with the reason in
// GetLastError (surfaced as the call's error result).
func internetSetOption(option uintptr) error {
	ret, _, err := procInternetSetOptionW.Call(0, option, 0, 0)
	if ret == 0 {
		return fmt.Errorf("InternetSetOption(%d): %w", option, err)
	}
	return nil
}
