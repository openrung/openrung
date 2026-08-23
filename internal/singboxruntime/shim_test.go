package singboxruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunSubcommandArgvContract pins the argv surface connectcore's
// SingBoxRunner invokes — `<binary> run -c <config>` — on the shared shim both
// the terminal client and the desktop app dispatch. A rename or a dropped -c
// would leave every tunnel start failing with a usage error.
func TestRunSubcommandArgvContract(t *testing.T) {
	if Subcommand != "run" {
		t.Fatalf("Subcommand = %q, but connectcore's runner invokes \"run\"", Subcommand)
	}

	err := RunSubcommand(nil)
	if err == nil || !strings.Contains(err.Error(), "-c") {
		t.Fatalf("run without -c: got %v, want the -c requirement", err)
	}

	// -c must reach the runtime: a path that does not exist has to surface as
	// the read error, not as a usage error or a wait on a signal.
	err = RunSubcommand([]string{"-c", filepath.Join(t.TempDir(), "absent.json")})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run with a missing config: got %v, want a not-exist error", err)
	}
}

// TestSelfPathIsThisExecutable guards what makes the shim work at all: the
// path the hosts hand the engine as SingBoxPath must be this process's own
// binary, since that is what re-execs into the run subcommand.
func TestSelfPathIsThisExecutable(t *testing.T) {
	got, err := SelfPath()
	if err != nil {
		t.Fatalf("SelfPath: %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got != want {
		t.Fatalf("SelfPath() = %q, want this executable %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("SelfPath() = %q, want an absolute path (the runner resolves it with exec.LookPath)", got)
	}
}

// TestVersionLineReportsTheBuildShape pins the line both release workflows
// assert against the go.mod pin. Version() is "unknown" in a test binary (it
// carries no dependency list), so the version half is checked for shape only;
// the build-tag half is the release gate — a package built without with_utls
// must say so instead of shipping an app that cannot dial any relay.
func TestVersionLineReportsTheBuildShape(t *testing.T) {
	got := VersionLine()
	if !strings.HasPrefix(got, "sing-box/") {
		t.Fatalf("VersionLine() = %q, want a leading \"sing-box/\"", got)
	}
	if strings.Contains(got, "sing-box/v") {
		t.Fatalf("VersionLine() = %q, want the version without its leading v", got)
	}
	if UTLSEnabled {
		if !strings.Contains(got, "(bundled, with_utls)") {
			t.Fatalf("VersionLine() = %q, want the with_utls label", got)
		}
		return
	}
	if !strings.Contains(got, "NO with_utls") || !strings.Contains(got, "rebuild with -tags with_utls") {
		t.Fatalf("VersionLine() = %q, want the missing-tag warning and its remedy", got)
	}
}
