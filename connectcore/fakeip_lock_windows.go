//go:build windows

// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockFakeIPFile(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errFakeIPCacheInUse
	}
	return err
}
