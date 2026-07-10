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
// locations — same problem (and same fix) as the desktop client's sing-box
// resolution in desktop/toolpath.go.

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

// xrayBinaryName is the platform's xray filename ("xray.exe" on Windows).
func xrayBinaryName() string {
	if runtime.GOOS == "windows" {
		return "xray.exe"
	}
	return "xray"
}

// resolveXrayPath finds the xray binary, preferring one bundled next to the
// app (Linux/Windows) or in the macOS .app's Contents/Resources, then PATH,
// then the common install directories. found=false falls back to the bare
// name so a start attempt surfaces a clear error, and lets the UI warn first.
func resolveXrayPath() (path string, found bool) {
	name := xrayBinaryName()
	// 1. Bundled alongside the executable.
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, rel := range []string{name, filepath.Join("..", "Resources", name)} {
			candidate := filepath.Join(dir, rel)
			if isExecutableFile(candidate) {
				return candidate, true
			}
		}
	}
	// 2. PATH (already augmented by ensureExternalToolPath).
	if resolved, err := exec.LookPath("xray"); err == nil {
		return resolved, true
	}
	// 3. Common install locations, in case PATH augmentation missed them.
	for _, dir := range commonToolDirs() {
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return name, false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
