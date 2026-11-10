// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// fitWindowToScreen shortens the window on a screen that cannot show the full
// phone height (a 768-pixel-tall laptop panel, for one). The width never
// changes, so the layout only loses vertical room for the map.
func fitWindowToScreen(ctx context.Context) {
	screens, err := wailsruntime.ScreenGetAll(ctx)
	if err != nil {
		return
	}
	screenHeight := 0
	for _, screen := range screens {
		if screen.IsCurrent || (screenHeight == 0 && screen.IsPrimary) {
			screenHeight = screen.Size.Height
		}
	}
	if height := phoneWindowHeightFor(screenHeight); height != phoneWindowHeight {
		wailsruntime.WindowSetSize(ctx, phoneWindowWidth, height)
		wailsruntime.WindowCenter(ctx)
	}
}
