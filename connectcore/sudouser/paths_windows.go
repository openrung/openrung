//go:build windows

package sudouser

import (
	"fmt"
	"os"
	"path/filepath"
)

// sudo ownership handoff is Unix-only. These implementations preserve normal
// Windows file behavior while keeping the shared packages portable.
func MkdirAll(dir string, perm os.FileMode) error {
	return os.MkdirAll(dir, perm)
}

func OpenDir(path string) (*os.File, error) {
	return os.Open(path)
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

func ChownRegularFileAt(dir *os.File, name string) error {
	if !Active() {
		return nil
	}
	file, err := OpenRegularFile(filepath.Join(dir.Name(), name), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return ChownFile(file)
}
