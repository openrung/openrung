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

const directoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW

// MkdirAll is os.MkdirAll, except every directory it actually creates is
// handed to the invoking user when Active. Elevated traversal is performed
// component by component relative to open directory descriptors so symlinks
// and pathname swaps cannot redirect creation or chown outside the config
// tree.
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
		next, openErr := openDirAt(current, component, nextDisplay)
		created := false
		if openErr != nil {
			if !errors.Is(openErr, syscall.ENOENT) {
				return openErr
			}
			if mkdirErr := unix.Mkdirat(int(current.Fd()), component, uint32(perm.Perm())); mkdirErr != nil {
				if !errors.Is(mkdirErr, syscall.EEXIST) {
					return &os.PathError{Op: "mkdirat", Path: nextDisplay, Err: mkdirErr}
				}
			} else {
				created = true
			}
			next, openErr = openDirAt(current, component, nextDisplay)
			if openErr != nil {
				return openErr
			}
		}
		if created {
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

// OpenDir opens a directory without following any symlink in its path.
func OpenDir(path string) (*os.File, error) {
	current, components, err := traversalRoot(path)
	if err != nil {
		return nil, err
	}
	display := current.Name()
	for _, component := range components {
		nextDisplay := filepath.Join(display, component)
		next, err := openDirAt(current, component, nextDisplay)
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

// OpenRegularFile opens a regular file without following any symlink in its
// final component. Elevated callers also reject symlinks in parent components.
func OpenRegularFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	if !Active() {
		fd, err := unix.Open(path, flag|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
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
	uid, gid, active := ids()
	if !active {
		return nil
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid directory entry %q", name)
	}
	file, err := openRegularFileAt(dir, name, filepath.Join(dir.Name(), name), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return fileChown(file, uid, gid)
}

func traversalRoot(path string) (*os.File, []string, error) {
	clean := filepath.Clean(path)
	root := "."
	remainder := clean
	if filepath.IsAbs(clean) {
		root = string(filepath.Separator)
		remainder = strings.TrimPrefix(clean, root)
	}
	fd, err := unix.Open(root, directoryOpenFlags, 0)
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

func openDirAt(parent *os.File, name, display string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, directoryOpenFlags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	return os.NewFile(uintptr(fd), display), nil
}

func openRegularFileAt(parent *os.File, name, display string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flag|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
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
