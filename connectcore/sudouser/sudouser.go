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
	"path/filepath"
	"strconv"
)

// The euid check and the chown syscall are package vars so tests can exercise
// the elevated path without running as root.
var (
	geteuid = os.Geteuid
	chown   = os.Chown
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

// Chown hands path to the invoking user when Active, and is a no-op
// otherwise.
func Chown(path string) error {
	uid, gid, ok := ids()
	if !ok {
		return nil
	}
	return chown(path, uid, gid)
}

// MkdirAll is os.MkdirAll, except every directory it actually creates is
// handed to the invoking user when Active — a sudo'd first run must not leave
// root-owned directories inside the user's home.
func MkdirAll(dir string, perm os.FileMode) error {
	var created []string
	for cur := dir; ; {
		if _, err := os.Stat(cur); err == nil || !os.IsNotExist(err) {
			break
		}
		created = append(created, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	// Deepest-last so parents are chowned before their children.
	for i := len(created) - 1; i >= 0; i-- {
		if err := Chown(created[i]); err != nil {
			return err
		}
	}
	return nil
}
