//go:build !windows

package clienttelemetry

import (
	"errors"
	"os"
	"syscall"
)

// lockOutboxFile takes the advisory exclusive cross-process lock on the
// outbox's lock file, without blocking: a second process degrades to an
// unavailable outbox instead of waiting on (or clobbering) the owner. The
// kernel releases the lock on process death, which is the crash-safety the
// mobile outboxes need.
func lockOutboxFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockOutboxFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

// syncOutboxDir flushes the directory entry after a rename or first append,
// so "durably persisted" includes the file's very existence.
func syncOutboxDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}
