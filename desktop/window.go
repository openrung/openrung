// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// fitWindowToScreen shrinks the opening window on a screen smaller than the
// default size (a 768-pixel-tall laptop panel, for one). The user can resize
// it freely from there.
func fitWindowToScreen(ctx context.Context) {
	screenWidth, screenHeight := 0, 0
	if screens, err := wailsruntime.ScreenGetAll(ctx); err == nil {
		for _, screen := range screens {
			if screen.IsCurrent || (screenHeight == 0 && screen.IsPrimary) {
				screenWidth, screenHeight = screen.Size.Width, screen.Size.Height
			}
		}
	}
	width, height := windowSizeFor(screenWidth, screenHeight)
	if width != defaultWindowWidth || height != defaultWindowHeight {
		wailsruntime.WindowSetSize(ctx, width, height)
		wailsruntime.WindowCenter(ctx)
	}
}
