// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"errors"
	"fmt"
	"os"

	"github.com/openrung/openrung/connectcore/sudouser"
)

var errFakeIPCacheInUse = errors.New("another VPN connection is already using this installation; disconnect it first")

// acquireFakeIPLock owns the cache and run-state lifecycle, including resets
// before sing-box opens its database. Keep the separate lock file in place:
// deleting or replacing it would let callers lock different files at one path.
// Closing the handle (or process exit) releases ownership.
func acquireFakeIPLock(cachePath string) (*os.File, error) {
	file, err := sudouser.OpenRegularFile(cachePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open VPN ownership lock: %w", err)
	}
	if err := lockFakeIPFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire VPN ownership lock: %w", err)
	}
	return file, nil
}
