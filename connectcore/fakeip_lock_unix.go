//go:build !windows

// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockFakeIPFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errFakeIPCacheInUse
	}
	return err
}
