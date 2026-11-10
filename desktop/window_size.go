// SPDX-License-Identifier: GPL-3.0-or-later

package main

// The app's UI is the mobile app's, laid out for a phone screen, so the window
// is a fixed, non-resizable slab of phone proportions rather than a freely
// sized desktop window.
const (
	phoneWindowWidth     = 390
	phoneWindowHeight    = 844
	minPhoneWindowHeight = 600

	// screenChromeAllowance is the vertical room left for what surrounds the
	// window's content: its own title bar plus the system's menu bar, dock or
	// taskbar.
	screenChromeAllowance = 140
)

// phoneWindowHeightFor returns the content height to use on a screen of the
// given logical height: the full phone height where it fits, otherwise as
// much as the screen allows, down to a floor below which the layout would
// not hold the home screen's controls. An unknown screen (0) gets the default.
func phoneWindowHeightFor(screenHeight int) int {
	if screenHeight <= 0 {
		return phoneWindowHeight
	}
	return min(phoneWindowHeight, max(minPhoneWindowHeight, screenHeight-screenChromeAllowance))
}
