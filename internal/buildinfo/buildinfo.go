// Package buildinfo resolves the version identity every OpenRung binary
// reports through -version flags, startup logs, and telemetry.
//
// Release builds inject the identity with linker flags:
//
//	go build -ldflags "-X openrung/internal/buildinfo.version=X.Y.Z -X openrung/internal/buildinfo.revision=<commit>"
//
// (see deploy/*/Dockerfile and .github/workflows/client-release.yml). When the
// injection is absent — plain `go build`, `go run`, tests — Version falls back
// to the component's embedded VERSION file and Revision to the VCS metadata
// the Go toolchain stamps into the binary, so development builds still report
// something truthful instead of an empty string.
package buildinfo

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// version and revision are overridden by release builds using -ldflags. The
// injection_test.go probe proves these symbol paths still resolve; the linker
// silently ignores -X for a symbol it cannot find.
var (
	version  string
	revision string
)

// Version returns the injected release version, falling back to the
// component's embedded VERSION file contents, then "dev".
func Version(embedded string) string {
	if value := strings.TrimSpace(version); value != "" {
		return value
	}
	if value := strings.TrimSpace(embedded); value != "" {
		return value
	}
	return "dev"
}

// Revision returns the injected VCS revision, falling back to the revision
// recorded by the Go toolchain (with a "-dirty" suffix for modified trees),
// then "unknown".
func Revision() string {
	if value := strings.TrimSpace(revision); value != "" {
		return value
	}
	if value := vcsRevision(); value != "" {
		return value
	}
	return "unknown"
}

// Info renders the standard "<component>/<version> revision=<revision>" line
// printed by every -version flag.
func Info(component, embedded string) string {
	return fmt.Sprintf("%s/%s revision=%s", component, Version(embedded), Revision())
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if rev == "" {
		return ""
	}
	if modified == "true" {
		return rev + "-dirty"
	}
	return rev
}
