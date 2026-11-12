//go:build !darwin && !windows && !linux

package proxymode

import "errors"

// New returns a not-yet-supported controller on platforms without a proxy
// implementation (macOS, Windows and Linux have one); the app advertises the
// loopback proxy address for manual configuration instead.
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
