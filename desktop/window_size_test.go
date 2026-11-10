// SPDX-License-Identifier: GPL-3.0-or-later

package main

import "testing"

func TestPhoneWindowHeightFor(t *testing.T) {
	for _, tc := range []struct {
		name         string
		screenHeight int
		want         int
	}{
		{"unknown screen keeps the default", 0, phoneWindowHeight},
		{"tall screen shows the full phone height", 1440, phoneWindowHeight},
		{"exactly enough room", phoneWindowHeight + screenChromeAllowance, phoneWindowHeight},
		{"one pixel short shrinks", phoneWindowHeight + screenChromeAllowance - 1, phoneWindowHeight - 1},
		{"900-tall laptop", 900, 760},
		{"768-tall laptop", 768, 628},
		{"very short screen stops at the floor", 600, minPhoneWindowHeight},
	} {
		if got := phoneWindowHeightFor(tc.screenHeight); got != tc.want {
			t.Errorf("%s: phoneWindowHeightFor(%d) = %d, want %d", tc.name, tc.screenHeight, got, tc.want)
		}
	}
}
