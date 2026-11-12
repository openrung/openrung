// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import "strings"

const (
	desktopCaptureModeFile = "desktop-capture-mode"
	desktopCaptureModeTUN  = "tun"
)

// LoadDesktopTUNMode reports whether the desktop app was last set to capture
// the whole device through a TUN interface. A missing, unreadable or
// unrecognized file reads as proxy mode, the mode that needs no privileges.
// The terminal client takes its mode from a flag and never reads this.
func (s *Store) LoadDesktopTUNMode() bool {
	data, err := s.readFile(desktopCaptureModeFile)
	return err == nil && strings.TrimSpace(string(data)) == desktopCaptureModeTUN
}

// SaveDesktopTUNMode persists the desktop app's capture mode.
func (s *Store) SaveDesktopTUNMode(tun bool) error {
	mode := "proxy"
	if tun {
		mode = desktopCaptureModeTUN
	}
	return s.writeFile(desktopCaptureModeFile, []byte(mode+"\n"))
}
