//go:build linux

package directsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeLinuxFiles map[string]string

func (f fakeLinuxFiles) ReadFile(name string) ([]byte, error) {
	value, ok := f[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(value), nil
}

type fakeCommandResult struct {
	output string
	err    error
}

type fakeCommandRunner struct {
	results []fakeCommandResult
	calls   []Command
}

func (r *fakeCommandRunner) Run(_ context.Context, command Command) ([]byte, error) {
	r.calls = append(r.calls, command)
	if len(r.results) == 0 {
		return nil, nil
	}
	result := r.results[0]
	r.results = r.results[1:]
	return []byte(result.output), result.err
}

type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return "exit failure" }
func (e fakeExitError) ExitCode() int { return e.code }

func testLinuxPlatform(runner CommandRunner, files fileReader) *linuxPlatform {
	return &linuxPlatform{
		executable: "/opt/OpenRung Volunteer/OpenRungVolunteer",
		getcapPath: "/usr/sbin/getcap",
		setcapPath: "/usr/sbin/setcap",
		pkexecPath: "/usr/bin/pkexec",
		runner:     runner,
		files:      files,
		firewall:   " firewall unchanged",
		securePath: func(string) error { return nil },
	}
}

func TestLinuxUnprivilegedLowPortNeedsNoElevation(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "443\n",
	})
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateReady || status.Reason != ReasonLowPortsUnprivileged {
		t.Fatalf("status = %+v, want host-level low-port ready", status)
	}
	if len(runner.calls) != 1 || runner.calls[0].Path != platform.getcapPath {
		t.Fatalf("host-level inspection calls = %+v, want read-only getcap only", runner.calls)
	}
}

func TestLinuxEnableUsesFixedHelperArgumentsAndRequiresRestart(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{}, // initial getcap: no capability
		{}, // pre-elevation recheck: no capability
		{}, // pkexec setcap succeeds
		{output: "/opt/OpenRung Volunteer/OpenRungVolunteer cap_net_bind_service=ep\n"},
	}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath:  "1024\n",
		linuxProcessStatusPath: "Name:\tOpenRung\nCapEff:\t0000000000000000\n",
	})
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateNeedsSetup || status.Reason != ReasonRestartRequired || status.CanEnable {
		t.Fatalf("status = %+v, want restart_required", status)
	}
	if !strings.Contains(status.Message, "Quit and reopen") {
		t.Fatalf("restart message = %q", status.Message)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("commands = %+v, want inspect/setup/inspect", runner.calls)
	}
	setup := runner.calls[2]
	want := []string{platform.executable, linuxHelperEnableFlag}
	if setup.Path != "/usr/bin/pkexec" || len(setup.Args) != len(want) {
		t.Fatalf("setup command = %+v, want pkexec %+v", setup, want)
	}
	for i := range want {
		if setup.Args[i] != want[i] {
			t.Fatalf("setup argv[%d] = %q, want %q", i, setup.Args[i], want[i])
		}
	}
}

func TestLinuxExactCapabilityAndEffectiveBitAreReady(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath:  "1024\n",
		linuxProcessStatusPath: "CapEff:\t0000000000000400\n",
	})
	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateReady || !status.CanRemove {
		t.Fatalf("status = %+v, want active exact capability", status)
	}
}

func TestLinuxPkexecCancellation126IsRetryable(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{},
		{},
		{err: fakeExitError{code: 126}},
	}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "1024\n",
	})
	status, err := NewManagerWithPlatform("linux", platform).Enable(context.Background())
	if err == nil {
		t.Fatal("Enable unexpectedly succeeded")
	}
	if status.State != StateNeedsSetup || status.Reason != ReasonAuthorizationDeclined || !status.CanEnable {
		t.Fatalf("cancelled status = %+v", status)
	}
}

func TestLinuxUnsupportedCapabilityFilesystemBecomesUnavailable(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{},
		{},
		{err: fakeExitError{code: linuxHelperExitFilesystemUnsupported}},
	}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "1024\n",
	})
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err == nil {
		t.Fatal("Enable unexpectedly succeeded")
	}
	if status.State != StateUnavailable || status.Reason != ReasonFilesystemUnsupported {
		t.Fatalf("failure status = %+v", status)
	}
	refreshed := manager.Status(context.Background())
	if refreshed.State != StateUnavailable || refreshed.Reason != ReasonFilesystemUnsupported {
		t.Fatalf("refreshed status = %+v, want remembered unsupported filesystem", refreshed)
	}
}

func TestLinuxMismatchedCapabilitiesAreNotAccepted(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_admin,cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "1024\n",
	})
	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateUnavailable || status.Reason != ReasonCapabilityMismatch ||
		status.CanRemove || status.CanEnable {
		t.Fatalf("status = %+v, want fail-closed capability mismatch", status)
	}
}

func TestLinuxLowPortHostStillSurfacesExactCapabilityRemoval(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "443\n",
	})

	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateReady || !status.CanRemove {
		t.Fatalf("status = %+v, want unnecessary exact capability removable", status)
	}
	if !strings.Contains(status.Message, "unnecessary") {
		t.Fatalf("message = %q, want unnecessary-capability guidance", status.Message)
	}
}

func TestLinuxExactCapabilityWithoutRemovalToolsIsNotMarkedRemovable(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath:  "1024\n",
		linuxProcessStatusPath: "CapEff:\t0000000000000400\n",
	})
	platform.pkexecPath = ""

	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateReady || status.CanRemove {
		t.Fatalf("status = %+v, want ready but non-removable", status)
	}
}

func TestLinuxCapabilityMissingOnUserWritablePathFailsClosed(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath: "1024\n",
	})
	platform.securePath = func(string) error { return errors.New("binary is not owned by root") }

	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateUnavailable || status.Reason != ReasonInsecureExecutable ||
		status.CanEnable || status.CanRemove {
		t.Fatalf("status = %+v, want unsafe portable path unavailable", status)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %+v, want inspection only", runner.calls)
	}
}

func TestLinuxPreexistingIneffectiveCapabilityDoesNotRequestEndlessRestart(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath:  "1024\n",
		linuxProcessStatusPath: "CapEff:\t0000000000000000\n",
	})

	status := NewManagerWithPlatform("linux", platform).Status(context.Background())
	if status.State != StateUnavailable || status.Reason != ReasonCapabilityIneffective ||
		!status.CanRemove || status.CanEnable {
		t.Fatalf("status = %+v, want ineffective capability guidance", status)
	}
}

func TestLinuxRemoveRefusesUnknownCapabilitiesWithoutElevation(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/app/OpenRungVolunteer cap_net_admin,cap_net_bind_service=ep\n",
	}}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{})

	err := platform.Remove(context.Background())
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Reason != ReasonCapabilityMismatch {
		t.Fatalf("Remove error = %#v, want capability mismatch", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Path != platform.getcapPath {
		t.Fatalf("calls = %+v, want read-only getcap only", runner.calls)
	}
}

func TestLinuxPrivilegedHelperAcceptsOnlyFixedFlags(t *testing.T) {
	if operation, ok := linuxPrivilegedOperationForArgs([]string{linuxHelperEnableFlag}); !ok || operation != linuxPrivilegedEnable {
		t.Fatal("enable helper flag was not accepted")
	}
	if operation, ok := linuxPrivilegedOperationForArgs([]string{linuxHelperRemoveFlag}); !ok || operation != linuxPrivilegedRemove {
		t.Fatal("remove helper flag was not accepted")
	}
	for _, args := range [][]string{
		nil,
		{linuxHelperEnableFlag, "/tmp/untrusted"},
		{"--arbitrary-root-command"},
	} {
		if _, ok := linuxPrivilegedOperationForArgs(args); ok {
			t.Fatalf("unsafe helper arguments accepted: %#v", args)
		}
	}
}

func TestLinuxPrivilegedHelperRechecksBeforeExactGrant(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{},
		{},
	}}
	err := runLinuxPrivilegedOperation(
		context.Background(),
		linuxPrivilegedEnable,
		"/opt/openrung/OpenRungVolunteer",
		"/usr/sbin/getcap",
		"/usr/sbin/setcap",
		runner,
	)
	if err != nil {
		t.Fatalf("runLinuxPrivilegedOperation: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %+v, want getcap then setcap", runner.calls)
	}
	mutation := runner.calls[1]
	want := []string{linuxBindCapability, "/opt/openrung/OpenRungVolunteer"}
	if mutation.Path != "/usr/sbin/setcap" || !equalArguments(mutation.Args, want) {
		t.Fatalf("mutation = %+v, want setcap %v", mutation, want)
	}
}

func TestLinuxPrivilegedHelperRefusesCapabilityChangedDuringPrompt(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{{
		output: "/opt/openrung/OpenRungVolunteer cap_net_admin,cap_net_bind_service=ep\n",
	}}}
	err := runLinuxPrivilegedOperation(
		context.Background(),
		linuxPrivilegedEnable,
		"/opt/openrung/OpenRungVolunteer",
		"/usr/sbin/getcap",
		"/usr/sbin/setcap",
		runner,
	)
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("error = %v, want fail-closed mismatch", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %+v, want no privileged mutation", runner.calls)
	}
}

func TestLinuxPrivilegedHelperRemovesOnlyExactCapability(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: "/opt/openrung/OpenRungVolunteer cap_net_bind_service=ep\n"},
		{},
	}}
	err := runLinuxPrivilegedOperation(
		context.Background(),
		linuxPrivilegedRemove,
		"/opt/openrung/OpenRungVolunteer",
		"/usr/sbin/getcap",
		"/usr/sbin/setcap",
		runner,
	)
	if err != nil {
		t.Fatalf("runLinuxPrivilegedOperation: %v", err)
	}
	if got, want := runner.calls[1].Args, []string{"-r", "/opt/openrung/OpenRungVolunteer"}; !equalArguments(got, want) {
		t.Fatalf("remove args = %v, want %v", got, want)
	}
}

func TestLinuxPrivilegedHelperMapsUnsupportedFilesystemToFixedExit(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{},
		{
			output: "Failed to set capabilities: Operation not supported",
			err:    fakeExitError{code: 1},
		},
	}}
	err := runLinuxPrivilegedOperation(
		context.Background(),
		linuxPrivilegedEnable,
		"/opt/openrung/OpenRungVolunteer",
		"/usr/sbin/getcap",
		"/usr/sbin/setcap",
		runner,
	)
	if got := linuxHelperExitCode(err); got != linuxHelperExitFilesystemUnsupported {
		t.Fatalf("helper exit classification = %d, want %d (error %v)", got, linuxHelperExitFilesystemUnsupported, err)
	}
}

func TestLinuxRemovalRequiresRestartBeforeVolunteeringAgain(t *testing.T) {
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n"},
		{output: "/app/OpenRungVolunteer cap_net_bind_service=ep\n"},
		{},
	}}
	platform := testLinuxPlatform(runner, fakeLinuxFiles{
		linuxLowPortStartPath:  "1024\n",
		linuxProcessStatusPath: "CapEff:\t0000000000000400\n",
	})
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Remove(context.Background())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if status.State != StateUnavailable || status.Reason != ReasonRemovalRestartRequired ||
		status.CanEnable || status.CanRemove {
		t.Fatalf("status = %+v, want restart-gated removal", status)
	}
	if len(runner.calls) != 3 || runner.calls[2].Path != platform.pkexecPath ||
		!equalArguments(runner.calls[2].Args, []string{platform.executable, linuxHelperRemoveFlag}) {
		t.Fatalf("calls = %+v, want inspect/recheck/fixed removal helper", runner.calls)
	}
}

func TestLinuxSecurePathRejectsSetuidExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "OpenRungVolunteer")
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	if err := os.Chmod(path, 0o755|os.ModeSetuid); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}
	err := linuxSecureInstalledExecutable(path)
	if err == nil || !strings.Contains(err.Error(), "setuid") {
		t.Fatalf("validation error = %v, want setuid rejection", err)
	}
}

func equalArguments(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
