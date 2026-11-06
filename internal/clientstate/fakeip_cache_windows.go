// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package clientstate

import "os"

func fakeIPCacheLocation(stateDir string) (string, string, error) {
	return stateDir, "fakeip.db", nil
}

// Windows uses the user's config directory and its inherited account ACL.
func cacheDirectoryPrivate(os.FileInfo) bool { return true }
