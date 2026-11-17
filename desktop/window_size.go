// SPDX-License-Identifier: GPL-3.0-or-later

package main

// The window is an ordinary resizable desktop window. The UI (the mobile
// app's) lays itself out by width: its phone layout in a narrow window, and a
// navigation rail with a docked control panel beside the map in a wide one.
const (
	defaultWindowWidth  = 1180
	defaultWindowHeight = 780

	// The smallest window the phone layout still holds the home screen's
	// controls in.
	minWindowWidth  = 360
	minWindowHeight = 560

	// screenChromeAllowance is the room left around the window on a screen
	// smaller than the default size: its own title bar plus the system's menu
	// bar, dock or taskbar.
	screenChromeAllowance = 140
)

// windowSizeFor returns the size to open at on a screen of the given logical
// size: the default where it fits, otherwise as much as the screen allows,
// down to the minimum size. An unknown dimension (0) gets the default.
func windowSizeFor(screenWidth, screenHeight int) (width, height int) {
	return fitDimension(defaultWindowWidth, minWindowWidth, screenWidth),
		fitDimension(defaultWindowHeight, minWindowHeight, screenHeight)
}

func fitDimension(want, floor, screen int) int {
	if screen <= 0 {
		return want
	}
	return min(want, max(floor, screen-screenChromeAllowance))
}
