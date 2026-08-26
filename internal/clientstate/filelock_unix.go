//go:build !windows

// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"os"

	"golang.org/x/sys/unix"

	"github.com/openrung/openrung/connectcore/sudouser"
)

func withFileLock(path string, fn func() error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	// A lock file created during a sudo'd run must stay usable by the
	// invoking user's plain runs.
	if err := sudouser.ChownFile(file); err != nil {
		return err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN) //nolint:errcheck // best-effort unlock while closing
	return fn()
}
