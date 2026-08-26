// Package sudouser resolves the invoking user when the process was elevated
// with sudo (e.g. `sudo openrung-client --tun`). Per-install state lives under
// os.UserConfigDir()/openrung, which sudo resolves inside the invoking user's
// home while the effective uid is root — so directories and files created
// during an elevated run must be handed back to that user or every later
// plain run silently fails to read or write them.
//
// Every helper is a no-op when the process is not root, and for genuinely
// root-owned daemons, which run without SUDO_UID/SUDO_GID in the environment.
package sudouser

import (
	"os"
	"strconv"
)

// The euid check and the chown syscall are package vars so tests can exercise
// the elevated path without running as root.
var (
	geteuid   = os.Geteuid
	fileChown = func(file *os.File, uid, gid int) error {
		return file.Chown(uid, gid)
	}
)

// ids returns the invoking user's uid/gid when the process is root under
// sudo. ok is false for non-root processes and for genuinely-root daemons
// (no SUDO_UID), so callers can chown unconditionally.
func ids() (uid, gid int, ok bool) {
	if geteuid() != 0 {
		return 0, 0, false
	}
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil || uid <= 0 {
		return 0, 0, false
	}
	gid, err = strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil || gid < 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// Active reports whether the process is running as root on behalf of a
// non-root invoking user.
func Active() bool {
	_, _, ok := ids()
	return ok
}

// ChownFile hands an already-open file or directory to the invoking user when
// Active, and is a no-op otherwise. Using the descriptor rather than reopening
// a pathname prevents a user-controlled symlink swap from redirecting chown to
// a privileged target.
func ChownFile(file *os.File) error {
	uid, gid, ok := ids()
	if !ok {
		return nil
	}
	return fileChown(file, uid, gid)
}
