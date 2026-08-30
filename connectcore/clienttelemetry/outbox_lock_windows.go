//go:build windows

package clienttelemetry

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockOutboxFile is the Windows shape of the advisory cross-process lock:
// LockFileEx with fail-immediately semantics over the first byte of the lock
// file. The kernel releases it when the owning process dies, like flock.
// (The mobile platforms never take this path — gomobile targets are unix — but
// connectcore also runs inside the Windows desktop app.)
func lockOutboxFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0,
		new(windows.Overlapped),
	)
}

func unlockOutboxFile(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// syncOutboxDir is a no-op on Windows: NTFS metadata durability rides the
// file's own FlushFileBuffers (File.Sync), and directory handles cannot be
// flushed the way unix directory fds can.
func syncOutboxDir(string) error { return nil }
