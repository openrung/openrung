package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// GUI apps launched from Finder/launchd inherit a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) that omits Homebrew and other common install
// locations — so external tools like sing-box are invisible even when
// installed, which is why `wails dev` (launched from a shell, inheriting its
// PATH) worked but the double-clicked .app does not.

// commonToolDirs are the extra bin directories external tools typically live in.
func commonToolDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/opt/homebrew/bin", "/usr/local/bin"}
	case "linux":
		return []string{"/usr/local/bin", "/usr/bin"}
	default:
		return nil
	}
}

// ensureExternalToolPath appends the common tool directories to PATH so
// exec.LookPath behaves the same for a Finder-launched app as it does from a
// terminal.
func ensureExternalToolPath() {
	path := os.Getenv("PATH")
	existing := strings.Split(path, string(os.PathListSeparator))
	seen := make(map[string]struct{}, len(existing))
	for _, dir := range existing {
		seen[dir] = struct{}{}
	}
	for _, dir := range commonToolDirs() {
		if _, ok := seen[dir]; !ok {
			path += string(os.PathListSeparator) + dir
		}
	}
	os.Setenv("PATH", path)
}

// singBoxBinaryName is the platform's sing-box filename ("sing-box.exe" on
// Windows). exec.LookPath already resolves the extension via PATHEXT, but the
// explicit-path checks below need it spelled out.
func singBoxBinaryName() string {
	if runtime.GOOS == "windows" {
		return "sing-box.exe"
	}
	return "sing-box"
}

// resolveSingBoxPath finds the sing-box binary, preferring one bundled next to
// the app, then PATH, then the common install directories. It returns an
// absolute path when found; SingBoxRunner accepts a slash-bearing path
// directly. Falls back to the bare name so the runner surfaces a clear error.
func resolveSingBoxPath() string {
	name := singBoxBinaryName()
	// 1. Bundled alongside the executable — next to it (Linux/Windows) or in
	//    the macOS .app's Contents/Resources.
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, rel := range []string{name, filepath.Join("..", "Resources", name)} {
			candidate := filepath.Join(dir, rel)
			if isExecutableFile(candidate) {
				return candidate
			}
		}
	}
	// 2. PATH (already augmented by ensureExternalToolPath).
	if resolved, err := exec.LookPath("sing-box"); err == nil {
		return resolved
	}
	// 3. Common install locations, in case PATH augmentation missed them.
	for _, dir := range commonToolDirs() {
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	return name
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
