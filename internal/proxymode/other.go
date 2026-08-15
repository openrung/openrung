//go:build !darwin && !windows

package proxymode

import "errors"

// New returns a not-yet-supported controller on platforms without a proxy
// implementation. macOS (networksetup) and Windows (WinInet) are implemented;
// Linux (gsettings for GNOME, env vars elsewhere) lands in a later phase, until
// then the app advertises the loopback proxy address for manual configuration.
func New() Controller {
	return unsupportedController{}
}

type unsupportedController struct{}

func (unsupportedController) Supported() bool { return false }

func (unsupportedController) Snapshot() (Snapshot, error) {
	return Snapshot{}, nil
}

func (unsupportedController) Set(host string, port int) error {
	return errors.New("system proxy control is not implemented on this platform yet")
}

func (unsupportedController) Restore(snap Snapshot) error {
	return nil
}
