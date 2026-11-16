// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"runtime"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// sizeHeldByHints is true where the window cannot be shrunk while it is
// non-resizable: GTK keeps a non-resizable window at least the size it opened
// at, so on Linux the window stays resizable and fitWindowToScreen pins it with
// equal minimum and maximum sizes instead.
const sizeHeldByHints = runtime.GOOS == "linux"

// fitWindowToScreen shortens the window on a screen that cannot show the full
// phone height (a 768-pixel-tall laptop panel, for one). The width never
// changes, so the layout only loses vertical room for the map.
func fitWindowToScreen(ctx context.Context) {
	screenHeight := 0
	if screens, err := wailsruntime.ScreenGetAll(ctx); err == nil {
		for _, screen := range screens {
			if screen.IsCurrent || (screenHeight == 0 && screen.IsPrimary) {
				screenHeight = screen.Size.Height
			}
		}
	}
	height := phoneWindowHeightFor(screenHeight)
	if sizeHeldByHints {
		wailsruntime.WindowSetMinSize(ctx, phoneWindowWidth, height)
		wailsruntime.WindowSetMaxSize(ctx, phoneWindowWidth, height)
	}
	if height != phoneWindowHeight {
		wailsruntime.WindowSetSize(ctx, phoneWindowWidth, height)
		wailsruntime.WindowCenter(ctx)
	}
}
