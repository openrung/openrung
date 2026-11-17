// SPDX-License-Identifier: GPL-3.0-or-later

package main

import "testing"

func TestWindowSizeFor(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		screenWidth, screenHeight int
		wantWidth, wantHeight     int
	}{
		{"unknown screen keeps the default", 0, 0, defaultWindowWidth, defaultWindowHeight},
		{"large screen opens at the default", 2560, 1440, defaultWindowWidth, defaultWindowHeight},
		{"exactly enough room", defaultWindowWidth + screenChromeAllowance, defaultWindowHeight + screenChromeAllowance, defaultWindowWidth, defaultWindowHeight},
		{"one pixel short shrinks", defaultWindowWidth + screenChromeAllowance - 1, defaultWindowHeight + screenChromeAllowance - 1, defaultWindowWidth - 1, defaultWindowHeight - 1},
		{"1366x768 laptop", 1366, 768, defaultWindowWidth, 628},
		{"1024x768 screen", 1024, 768, 884, 628},
		{"tiny screen stops at the minimum", 400, 600, minWindowWidth, minWindowHeight},
	} {
		gotWidth, gotHeight := windowSizeFor(tc.screenWidth, tc.screenHeight)
		if gotWidth != tc.wantWidth || gotHeight != tc.wantHeight {
			t.Errorf("%s: windowSizeFor(%d, %d) = %dx%d, want %dx%d", tc.name,
				tc.screenWidth, tc.screenHeight, gotWidth, gotHeight, tc.wantWidth, tc.wantHeight)
		}
	}
}
