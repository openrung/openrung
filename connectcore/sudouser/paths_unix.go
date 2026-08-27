//go:build !windows

package sudouser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// Ancestors of the config directory are the invoking user's own layout and
	// may legitimately be symlinks — a home directory on another volume, or
	// ~/.config pointing into a dotfiles checkout — so traversal follows them.
	// The safety property is enforced where it matters instead: nothing is
	// handed to the invoking user unless it was opened O_NOFOLLOW and sits in
	// a directory that already belongs to them.
	dirOpenFlags         = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	dirOpenFlagsNoFollow = dirOpenFlags | unix.O_NOFOLLOW
	// Regular-file opens never follow a final symlink, and are non-blocking so
	// a FIFO left at a state path fails fast instead of hanging the client
	// inside open(2).
	fileOpenFlags = unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
)

// MkdirAll is os.MkdirAll, except every directory it actually creates is
// handed to the invoking user when Active. Creation walks the path component
// by component relative to open directory descriptors, and each new directory
// is re-opened O_NOFOLLOW before its ownership changes, so a pathname swap
// cannot redirect the chown outside the config tree.
func MkdirAll(dir string, perm os.FileMode) error {
	uid, gid, active := ids()
	if !active {
		return os.MkdirAll(dir, perm)
	}

	current, components, err := traversalRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()

	display := current.Name()
	for _, component := range components {
		nextDisplay := filepath.Join(display, component)
		// A component that already exists is only traversed, never chowned,
		// so following a symlink here hands the user nothing.
		next, openErr := openDirAt(current, component, nextDisplay, false)
		created := false
		if openErr != nil {
			if !errors.Is(openErr, syscall.ENOENT) {
				return openErr
			}
			if mkdirErr := unix.Mkdirat(int(current.Fd()), component, uint32(perm.Perm())); mkdirErr != nil {
				if !errors.Is(mkdirErr, syscall.EEXIST) {
					return &os.PathError{Op: "mkdirat", Path: nextDisplay, Err: mkdirErr}
				}
				// Lost the race to another process; treat the winner's
				// directory as pre-existing.
				next, openErr = openDirAt(current, component, nextDisplay, false)
			} else {
				created = true
				next, openErr = openDirAt(current, component, nextDisplay, true)
			}
			if openErr != nil {
				return openErr
			}
		}
		if created && ownedBy(current, uid) {
			// The gate matters when an ancestor symlink points outside the
			// user's tree: handing them a directory inside, say, /etc would
			// grant access they did not already have.
			if err := fileChown(next, uid, gid); err != nil {
				next.Close()
				return err
			}
		}
		if err := current.Close(); err != nil {
			next.Close()
			return err
		}
		current = next
		display = nextDisplay
	}
	return nil
}

// OpenDir opens a directory. Ancestors may be symlinks; the final component
// may not, so a caller holding the descriptor knows which directory it has.
func OpenDir(path string) (*os.File, error) {
	current, components, err := traversalRoot(path)
	if err != nil {
		return nil, err
	}
	display := current.Name()
	for i, component := range components {
		nextDisplay := filepath.Join(display, component)
		next, err := openDirAt(current, component, nextDisplay, i == len(components)-1)
		if err != nil {
			current.Close()
			return nil, err
		}
		if err := current.Close(); err != nil {
			next.Close()
			return nil, err
		}
		current = next
		display = nextDisplay
	}
	return current, nil
}

// OpenStateDir opens the client state directory for repair and, when Active,
// hands it back to the invoking user. The directory itself is never reached
// through a symlink, and ownership only changes when the containing directory
// already belongs to that user.
func OpenStateDir(dir string) (*os.File, error) {
	uid, gid, active := ids()
	if !active {
		return OpenDir(dir)
	}
	parent, err := OpenDir(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := openDirAt(parent, filepath.Base(dir), dir, true)
	if err != nil {
		return nil, err
	}
	if ownedBy(parent, uid) {
		if err := fileChown(file, uid, gid); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}

// OpenRegularFile opens a regular file without following a symlink in its
// final component. Elevated callers also pin the containing directory.
func OpenRegularFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	if !Active() {
		fd, err := unix.Open(path, flag|fileOpenFlags, uint32(perm.Perm()))
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return regularFileFromFD(fd, path)
	}
	parent, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return openRegularFileAt(parent, filepath.Base(path), path, flag, perm)
}

// ChownRegularFileAt hands a regular child of dir to the invoking user. dir is
// an already-open directory, so renaming or replacing its pathname cannot
// redirect the ownership change.
func ChownRegularFileAt(dir *os.File, name string) error {
	uid, _, active := ids()
	if !active {
		return nil
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid directory entry %q", name)
	}
	if !ownedBy(dir, uid) {
		return nil
	}
	file, err := openRegularFileAt(dir, name, filepath.Join(dir.Name(), name), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return ChownFile(file)
}

// ownedBy reports whether an open file or directory already belongs to uid.
func ownedBy(file *os.File, uid int) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

// hasExtraLinks reports whether a regular file has more than one name. A hard
// link planted at a state path resolves to a file the invoking user may not
// own, and O_NOFOLLOW does not catch it; unreadable metadata fails closed.
// Directories always carry several links, so they are exempt.
func hasExtraLinks(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink > 1
}

func traversalRoot(path string) (*os.File, []string, error) {
	clean := filepath.Clean(path)
	root := "."
	remainder := clean
	if filepath.IsAbs(clean) {
		root = string(filepath.Separator)
		remainder = strings.TrimPrefix(clean, root)
	}
	fd, err := unix.Open(root, dirOpenFlags, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	file := os.NewFile(uintptr(fd), root)
	if remainder == "" || remainder == "." {
		return file, nil, nil
	}
	components := strings.Split(remainder, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			file.Close()
			return nil, nil, fmt.Errorf("unsafe path component %q in %q", component, path)
		}
	}
	return file, components, nil
}

func openDirAt(parent *os.File, name, display string, nofollow bool) (*os.File, error) {
	flags := dirOpenFlags
	if nofollow {
		flags = dirOpenFlagsNoFollow
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	return os.NewFile(uintptr(fd), display), nil
}

func openRegularFileAt(parent *os.File, name, display string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flag|fileOpenFlags, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	return regularFileFromFD(fd, display)
}

func regularFileFromFD(fd int, display string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), display)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, &os.PathError{Op: "openat", Path: display, Err: fmt.Errorf("not a regular file")}
	}
	return file, nil
}
