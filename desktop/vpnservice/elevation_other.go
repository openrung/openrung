// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package vpnservice

import "errors"

// The desktop app offers TUN mode on Windows only (see tunnelmode.go): it has
// no way to obtain the rights for it on macOS or Linux, where the terminal
// client run as root covers the mode.
var (
	tunSupported     = false
	processElevated  = func() bool { return false }
	relaunchElevated = func() error {
		return errors.New("TUN mode is not available in the desktop app on this system")
	}
)

func waitForProcessExit(uint32) {}
