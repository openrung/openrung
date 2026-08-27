//go:build !windows

package sudouser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	return openDirChain(path, true)
}

// openDirChain walks path component by component. nofollowFinal pins the last
// component, which is the one an attacker names; ancestors are the user's own
// layout and are always followed.
func openDirChain(path string, nofollowFinal bool) (*os.File, error) {
	current, components, err := traversalRoot(path)
	if err != nil {
		return nil, err
	}
	display := current.Name()
	for i, component := range components {
		nextDisplay := filepath.Join(display, component)
		next, err := openDirAt(current, component, nextDisplay, nofollowFinal && i == len(components)-1)
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

// StateDir is an open handle to the client state directory. The directory is
// pinned by descriptor, so every repair and write below runs against the
// directory validated at open time rather than whatever the pathname resolves
// to later.
type StateDir struct {
	file *os.File
	// parentOwned records, at open time, whether the containing directory
	// already belonged to the invoking user. Handing them anything inside a
	// tree they do not own would grant access they did not already have.
	parentOwned bool
}

// OpenStateDir opens the client state directory, refusing to reach it through
// a symlink in its final component. A failure here is fatal to the caller: a
// state directory that resolves elsewhere would have root writing the invoking
// user's files into a privileged tree. The parent path is followed, so a home
// on another volume or a ~/.config pointing into a dotfiles checkout works.
func OpenStateDir(path string) (*StateDir, error) {
	parent, err := openDirChain(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := openDirAt(parent, filepath.Base(path), path, true)
	if err != nil {
		return nil, err
	}
	dir := &StateDir{file: file}
	if uid, _, active := ids(); active {
		dir.parentOwned = ownedBy(parent, uid)
	}
	return dir, nil
}

// Close releases the pinned directory.
func (d *StateDir) Close() error { return d.file.Close() }

// Entries lists the directory contents.
func (d *StateDir) Entries() ([]os.DirEntry, error) { return d.file.ReadDir(-1) }

// RepairOwner hands the directory itself back to the invoking user. Callers
// treat failure as best-effort: it costs one root-owned directory, where
// refusing to continue would cost the caller its whole store.
func (d *StateDir) RepairOwner() error {
	if !d.parentOwned {
		return nil
	}
	return ChownFile(d.file)
}

// RepairEntry hands one regular child back to the invoking user. The entry is
// opened relative to the pinned directory and never through a symlink, and a
// multiply-linked file is refused before any descriptor is returned.
func (d *StateDir) RepairEntry(name string) error {
	uid, _, active := ids()
	if !active {
		return nil
	}
	if !validStateName(name) {
		return fmt.Errorf("invalid directory entry %q", name)
	}
	if !ownedBy(d.file, uid) {
		return nil
	}
	file, err := openRegularFileAt(d.file, name, filepath.Join(d.file.Name(), name), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return ChownFile(file)
}

// WriteFile atomically replaces one state file, relative to the pinned
// directory throughout: the temporary file is created, written and renamed
// with *at syscalls, so swapping the directory's pathname for a symlink after
// it was validated cannot redirect the write.
func (d *StateDir) WriteFile(name string, data []byte, perm os.FileMode) error {
	if !validStateName(name) {
		return fmt.Errorf("invalid state file %q", name)
	}
	fd := int(d.file.Fd())
	// Qualifying by pid keeps concurrent client processes from colliding, the
	// same property os.CreateTemp provides on the unelevated path.
	tmp := "." + name + ".tmp-" + strconv.Itoa(os.Getpid())
	cleanup := func() { _ = unix.Unlinkat(fd, tmp, 0) }

	file, err := openRegularFileAt(d.file, tmp, filepath.Join(d.file.Name(), tmp), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := writeAndClose(file, data, perm); err != nil {
		cleanup()
		return err
	}
	if err := unix.Renameat(fd, tmp, fd, name); err != nil {
		cleanup()
		return &os.PathError{Op: "renameat", Path: filepath.Join(d.file.Name(), name), Err: err}
	}
	return nil
}

func writeAndClose(file *os.File, data []byte, perm os.FileMode) error {
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := ChownFile(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validStateName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

// OpenRegularFile opens a regular file without following a symlink in its
// final component. Elevated callers also pin the containing directory.
func OpenRegularFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	if !Active() {
		fd, err := unix.Open(path, (flag&^os.O_TRUNC)|fileOpenFlags, uint32(perm.Perm()))
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return regularFileFromFD(fd, path, flag&os.O_TRUNC != 0)
	}
	parent, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return openRegularFileAt(parent, filepath.Base(path), path, flag, perm)
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

// hasExtraLinks reports whether a regular file has more than one name.
// Unreadable metadata fails closed. Directories always carry several links, so
// they are exempt.
func hasExtraLinks(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return linkCount(info) > 1
}

// linkCount returns how many names a file has, or 2 — an impossible value for
// our state files — when the metadata cannot be read, so callers fail closed.
func linkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 2
	}
	return uint64(stat.Nlink)
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
	// O_TRUNC is withheld from the open: truncation is destructive, and the
	// file has not been proven to be a plain, singly-linked state file yet.
	fd, err := unix.Openat(int(parent.Fd()), name, (flag&^os.O_TRUNC)|fileOpenFlags, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	return regularFileFromFD(fd, display, flag&os.O_TRUNC != 0)
}

// regularFileFromFD validates a freshly opened descriptor before any caller
// can mutate through it, then applies a withheld truncation. An elevated run
// also refuses a multiply-linked file: a hard link planted at a state path
// reaches a file the invoking user may not own, and O_NOFOLLOW cannot see it.
func regularFileFromFD(fd int, display string, truncate bool) (*os.File, error) {
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
	if Active() && linkCount(info) > 1 {
		file.Close()
		return nil, &os.PathError{Op: "openat", Path: display, Err: fmt.Errorf("unexpected hard links")}
	}
	if truncate {
		if err := file.Truncate(0); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}
