//go:build windows

package sudouser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// sudo ownership handoff is Unix-only. These implementations preserve normal
// Windows file behavior while keeping the shared packages portable.
func MkdirAll(dir string, perm os.FileMode) error {
	return os.MkdirAll(dir, perm)
}

func OpenDir(path string) (*os.File, error) {
	return os.Open(path)
}

// StateDir mirrors the Unix handle. Ownership handoff never runs on Windows,
// because Active is always false, so the repairs are no-ops and writes keep
// their ordinary pathname semantics.
type StateDir struct {
	file *os.File
	dir  string
}

func OpenStateDir(path string) (*StateDir, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &StateDir{file: file, dir: path}, nil
}

func (d *StateDir) Close() error { return d.file.Close() }

func (d *StateDir) Entries() ([]os.DirEntry, error) { return d.file.ReadDir(-1) }

func (d *StateDir) RepairOwner() error { return nil }

func (d *StateDir) RepairEntry(name string) error { return nil }

func (d *StateDir) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.dir, name))
}

func (d *StateDir) Remove(name string) error {
	return os.Remove(filepath.Join(d.dir, name))
}

func (d *StateDir) WriteFile(name string, data []byte, perm os.FileMode) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid state file %q", name)
	}
	tmp := filepath.Join(d.dir, "."+name+".tmp-"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(d.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// hasExtraLinks has no Windows equivalent to guard: ownership handoff never
// runs there, because Active is always false.
func hasExtraLinks(file *os.File) bool {
	return false
}

func OpenRegularFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return file, nil
}
