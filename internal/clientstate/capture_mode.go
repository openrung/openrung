// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import "strings"

const (
	desktopCaptureModeFile  = "desktop-capture-mode"
	desktopCaptureModeTUN   = "tun"
	desktopCaptureModeProxy = "proxy"
)

// LoadDesktopTUNMode returns the capture mode the desktop app was last set
// to: tun says whether that was whole-device TUN capture, and stored is false
// when no choice is on disk (a first launch, or a missing, unreadable or
// unrecognized file), which leaves the starting mode to the app. The terminal
// client takes its mode from a flag and never reads this.
func (s *Store) LoadDesktopTUNMode() (tun, stored bool) {
	data, err := s.readFile(desktopCaptureModeFile)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(data)) {
	case desktopCaptureModeTUN:
		return true, true
	case desktopCaptureModeProxy:
		return false, true
	default:
		return false, false
	}
}

// SaveDesktopTUNMode persists the desktop app's capture mode.
func (s *Store) SaveDesktopTUNMode(tun bool) error {
	mode := desktopCaptureModeProxy
	if tun {
		mode = desktopCaptureModeTUN
	}
	return s.writeFile(desktopCaptureModeFile, []byte(mode+"\n"))
}
