// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package vpnservice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// relaunchParentWaitMS bounds, in milliseconds, how long an elevated instance
// waits for the instance that started it. That instance quits as soon as the
// start succeeds.
const relaunchParentWaitMS = 10_000

// tunSupported: Windows is the one desktop system on which the app can obtain
// the rights TUN mode needs by itself (see tunnelmode.go).
var tunSupported = true

// processElevated reports whether this process may create the tunnel adapter,
// which on Windows takes an elevated token.
var processElevated = func() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// relaunchElevated starts a second instance of this executable through the
// shell's "runas" verb, which shows the UAC prompt. The new instance is told
// this one's process id so it can wait for it to exit.
var relaunchElevated = func() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the app executable: %w", err)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args, _ := windows.UTF16PtrFromString(fmt.Sprintf("%s%d", relaunchFlag, os.Getpid()))
	dir, err := windows.UTF16PtrFromString(filepath.Dir(exe))
	if err != nil {
		return err
	}
	err = windows.ShellExecute(0, verb, file, args, dir, windows.SW_SHOWNORMAL)
	if errors.Is(err, windows.ERROR_CANCELLED) {
		return errors.New("TUN mode needs administrator rights, and the Windows prompt for them was declined")
	}
	if err != nil {
		return fmt.Errorf("restart with administrator rights: %w", err)
	}
	return nil
}

// waitForProcessExit blocks until the process exits, it cannot be opened
// (already gone), or the wait times out.
func waitForProcessExit(pid uint32) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_, _ = windows.WaitForSingleObject(handle, relaunchParentWaitMS)
}
