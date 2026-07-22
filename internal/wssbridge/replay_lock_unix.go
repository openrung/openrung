//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package wssbridge

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockReplayFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockReplayFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
