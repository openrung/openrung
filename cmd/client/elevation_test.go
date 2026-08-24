package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// The hook is the single authority on whether TUN mode may run here, and its
// error is the whole message the user gets — the engine passes it through to
// lastError untouched. It must therefore always say what to do next, and it
// must agree with the copy the Settings view shows.
func TestElevationRefusalCarriesActionableGuidance(t *testing.T) {
	err := elevation{}.Elevate(t.Context())

	// os.Geteuid reports -1 on Windows, so this only takes the root path where
	// root is a real thing.
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("Elevate as root: %v", err)
		}
		return
	}

	if err == nil {
		t.Fatal("Elevate did not refuse without the platform's TUN preconditions")
	}
	message := err.Error()
	if !strings.Contains(message, "proxy mode") {
		t.Fatalf("refusal names no fallback: %q", message)
	}
	switch runtime.GOOS {
	case "windows":
		// Windows is refused on teardown grounds, not privilege: telling the
		// user to elevate would send them in circles.
		if !strings.Contains(message, "not supported on Windows") {
			t.Fatalf("Windows refusal = %q; want the unsupported-platform reason", message)
		}
		if strings.Contains(strings.ToLower(message), "sudo") {
			t.Fatalf("Windows refusal mentions sudo: %q", message)
		}
	default:
		if !strings.Contains(message, "sudo") {
			t.Fatalf("unix refusal = %q; want the sudo rerun command", message)
		}
	}
}

// The Windows TUN gate admits only the bundled runtime: the stdin-close stop
// protocol is the only graceful-stop channel there, and an external -sing-box
// binary does not speak it. The goos parameter keeps every branch — including
// the refusal wording — testable from any platform.
func TestTUNRequiresBundledRuntimeOnWindowsOnly(t *testing.T) {
	if err := tunRequiresBundledRuntime("windows", false); err != nil {
		t.Fatalf("bundled runtime on Windows refused: %v", err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := tunRequiresBundledRuntime(goos, true); err != nil {
			t.Fatalf("external binary on %s refused: %v", goos, err)
		}
	}
	err := tunRequiresBundledRuntime("windows", true)
	if err == nil {
		t.Fatal("external binary on Windows admitted; its tunnel could never be stopped gracefully")
	}
	for _, want := range []string{"-sing-box", "proxy mode", "bundled"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q missing %q; it must name the cause and both ways out", err, want)
		}
	}
}

// The Settings row, the mode note, and the flag help all render these, so an
// empty one would show as a bare "TUN — whole device ()".
func TestTUNModeCopyIsPopulated(t *testing.T) {
	if strings.TrimSpace(tunModeSummary) == "" || strings.TrimSpace(tunModeAdvice) == "" {
		t.Fatalf("TUN mode copy is empty: summary=%q advice=%q", tunModeSummary, tunModeAdvice)
	}
	if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(tunModeSummary+tunModeAdvice), "sudo") {
		t.Fatalf("Windows UI copy tells the user to use sudo: %q / %q", tunModeSummary, tunModeAdvice)
	}
}
