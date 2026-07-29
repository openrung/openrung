package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Go linker silently ignores -X for a symbol it cannot resolve — the
// build still succeeds and the variable keeps its default. Renaming version
// or revision, or moving this package, would leave every release reporting
// its embedded VERSION fallback while every other check stayed green. Link a
// real probe with the flags release builds use and read the values back.
func TestReleaseLdflagsReachThisPackage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	ldflags := "-X openrung/internal/buildinfo.version=9.9.9 -X openrung/internal/buildinfo.revision=cafef00d"
	cmd := exec.Command("go", "run", "-ldflags", ldflags, "./versionprobe")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run versionprobe: %v\n%s", err, output)
	}
	got := strings.TrimSpace(string(output))
	want := "probe/9.9.9 revision=cafef00d"
	if got != want {
		t.Fatalf("versionprobe printed %q, want %q — the -X symbol paths used by release builds no longer resolve", got, want)
	}
}

// Release builds inject versions from several places outside the Go module
// graph. Keep every -X symbol they reference inside the set proven resolvable
// (this package's vars by the probe above; internal/client.appVersion by
// desktop/scripts/version-injection.test.mjs). A Dockerfile still pointing at
// the pre-buildinfo main.version symbol, or a typo in a new workflow, fails
// here instead of silently shipping fallback versions.
func TestInjectionSitesUseProvenSymbols(t *testing.T) {
	proven := map[string]bool{
		"openrung/internal/buildinfo.version":  true,
		"openrung/internal/buildinfo.revision": true,
		"openrung/internal/client.appVersion":  true,
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "*", "Dockerfile"))
	if err != nil {
		t.Fatalf("glob deploy Dockerfiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no deploy Dockerfiles found; the injection-site scan is not running")
	}
	workflows, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	files = append(files, workflows...)

	symbolPattern := regexp.MustCompile(`-X[ =]([A-Za-z0-9_./-]+\.[A-Za-z0-9_]+)=`)
	sawInjection := false
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, match := range symbolPattern.FindAllStringSubmatch(string(data), -1) {
			symbol := match[1]
			sawInjection = true
			if !proven[symbol] {
				t.Errorf("%s injects -X %s, which is not a symbol proven resolvable; the linker would silently ignore it", file, symbol)
			}
		}
	}
	if !sawInjection {
		t.Fatal("found no -X injection sites at all; the scan pattern is broken")
	}
}
