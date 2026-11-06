// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"fmt"
	"os"
	"path/filepath"
)

// FakeIPCachePath prepares private storage for sing-box's hostname/address
// cache. It owns neither the database nor allocation: sing-box opens and locks
// the same file for each run. Desktop and CLI use the same installation path.
func (s *Store) FakeIPCachePath() (string, error) {
	dir, name, err := fakeIPCacheLocation(s.dir)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || !cacheDirectoryPrivate(info) {
		return "", fmt.Errorf("DNS cache requires a private directory: %s", dir)
	}
	return filepath.Join(dir, name), nil
}
