// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package proxymode

import (
	"os"
	"os/exec"
)

// New returns the Linux proxy controller, which drives the session desktop's
// proxy setting (GSettings on GNOME-family desktops, kioslaverc on KDE). On
// any other desktop it reports unsupported, and the app advertises the
// loopback proxy address for manual configuration.
func New() Controller {
	return &linuxController{sys: linuxSystem{
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).Output()
		},
	}}
}
