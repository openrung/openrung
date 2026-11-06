// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !windows

package clientstate

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
)

func fakeIPCacheLocation(stateDir string) (string, string, error) {
	if os.Geteuid() != 0 {
		return stateDir, "fakeip.db", nil
	}
	// A privileged sing-box opens its database by pathname. Keep that path
	// under root's real home, not a sudo caller's writable state directory.
	// Scope the file by installation so desktop and CLI share it, without
	// sharing mappings between different users' installations.
	root, err := user.LookupId("0")
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(root.HomeDir)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !cacheDirectoryOwned(info) {
		return "", "", fmt.Errorf("DNS cache requires a protected root home")
	}
	installation, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return "", "", err
	}
	key := sha256.Sum256([]byte(installation))
	return filepath.Join(root.HomeDir, ".openrung-dns"), fmt.Sprintf("%x.db", key[:16]), nil
}

func cacheDirectoryOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func cacheDirectoryPrivate(info os.FileInfo) bool {
	return info.Mode().Perm()&0o077 == 0 && cacheDirectoryOwned(info)
}
