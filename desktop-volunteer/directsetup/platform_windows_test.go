//go:build windows

package directsetup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

type fakeWindowsRunner struct {
	output []byte
	err    error
	calls  []Command
}

type fakeWindowsExitError struct{ code int }

func (e fakeWindowsExitError) Error() string { return "exit failure" }
func (e fakeWindowsExitError) ExitCode() int { return e.code }

func (r *fakeWindowsRunner) Run(_ context.Context, command Command) ([]byte, error) {
	r.calls = append(r.calls, command)
	return r.output, r.err
}

func testWindowsEnvironment(executable string) []string {
	return []string{
		`SystemRoot=C:\Windows`,
		`WINDIR=C:\Windows`,
		`ComSpec=C:\Windows\System32\cmd.exe`,
		windowsExecutableEnv + "=" + executable,
	}
}

func TestWindowsStatusPassesExecutableAsDataNotScript(t *testing.T) {
	runner := &fakeWindowsRunner{output: []byte("MISSING")}
	path := `C:\Users\Volunteer & Co\OpenRungVolunteer.exe`
	platform := &windowsPlatform{
		executable:    path,
		powershell:    `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		environment:   testWindowsEnvironment(path),
		runner:        runner,
		statusCommand: encodePowerShell(windowsFirewallStatusScript),
	}

	status := NewManagerWithPlatform("windows", platform).Status(context.Background())
	if status.State != StateNeedsSetup {
		t.Fatalf("status = %+v, want needs_setup", status)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d", len(runner.calls))
	}
	call := runner.calls[0]
	if strings.Contains(strings.Join(call.Args, " "), path) {
		t.Fatal("untrusted executable path was interpolated into PowerShell arguments")
	}
	if len(call.Env) != 4 || call.Env[3] != windowsExecutableEnv+"="+path {
		t.Fatalf("executable data env = %#v", call.Env)
	}
	for _, entry := range call.Env {
		if strings.HasPrefix(strings.ToUpper(entry), "PSMODULEPATH=") ||
			strings.HasPrefix(strings.ToUpper(entry), "USERPROFILE=") {
			t.Fatalf("unsafe inherited environment entry = %q", entry)
		}
	}
}

func TestWindowsEnableUsesOnlyFixedHelperFlag(t *testing.T) {
	runner := &fakeWindowsRunner{output: []byte("MISSING")}
	var elevatedFlag string
	platform := &windowsPlatform{
		executable:    `C:\Apps\OpenRungVolunteer.exe`,
		powershell:    `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		environment:   testWindowsEnvironment(`C:\Apps\OpenRungVolunteer.exe`),
		runner:        runner,
		statusCommand: encodePowerShell(windowsFirewallStatusScript),
		runElevated: func(_ context.Context, flag string) error {
			elevatedFlag = flag
			runner.output = []byte("READY")
			return nil
		},
	}

	status, err := NewManagerWithPlatform("windows", platform).Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateReady || elevatedFlag != windowsHelperEnableFlag {
		t.Fatalf("status/flag = %+v / %q", status, elevatedFlag)
	}
	if strings.Contains(elevatedFlag, platform.executable) {
		t.Fatal("elevated helper flag contains an executable path")
	}
}

func TestWindowsUACCancellationIsDistinct(t *testing.T) {
	err := classifyWindowsElevationError(windows.ERROR_CANCELLED, "install rule")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Reason != ReasonAuthorizationDeclined || opErr.Unavailable {
		t.Fatalf("classified error = %#v", err)
	}
	err = classifyWindowsElevationError(
		fmt.Errorf("launcher: %w", fakeWindowsExitError{code: windowsElevationCancelledExit}),
		"install rule",
	)
	if !errors.As(err, &opErr) || opErr.Reason != ReasonAuthorizationDeclined {
		t.Fatalf("launcher cancellation = %#v", err)
	}
}

func TestWindowsGUIStartupRejectsAdministrator(t *testing.T) {
	if err := validateWindowsGUIElevation(true); err == nil {
		t.Fatal("Administrator GUI startup was accepted")
	}
	if err := validateWindowsGUIElevation(false); err != nil {
		t.Fatalf("normal GUI startup was rejected: %v", err)
	}
}

func TestWindowsMovedExecutableRuleIsReplacedOnExplicitEnable(t *testing.T) {
	runner := &fakeWindowsRunner{output: []byte("MISMATCH")}
	elevationCalls := 0
	platform := &windowsPlatform{
		executable:    `D:\Moved\OpenRungVolunteer.exe`,
		powershell:    `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		environment:   testWindowsEnvironment(`D:\Moved\OpenRungVolunteer.exe`),
		runner:        runner,
		statusCommand: encodePowerShell(windowsFirewallStatusScript),
		runElevated: func(_ context.Context, flag string) error {
			elevationCalls++
			if flag != windowsHelperEnableFlag {
				t.Fatalf("helper flag = %q", flag)
			}
			runner.output = []byte("READY")
			return nil
		},
	}
	manager := NewManagerWithPlatform("windows", platform)

	before := manager.Status(context.Background())
	if before.Reason != ReasonFirewallRuleMismatch || !before.CanRemove {
		t.Fatalf("moved-rule status = %+v, want removable mismatch", before)
	}
	after, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if after.State != StateReady || elevationCalls != 1 {
		t.Fatalf("after/calls = %+v / %d", after, elevationCalls)
	}
}

func TestWindowsFirewallOperationAcceptsOnlyFixedValues(t *testing.T) {
	if script, ok := privilegedScriptForArgs([]string{windowsHelperEnableFlag}); !ok || script != windowsFirewallEnableScript {
		t.Fatal("enable helper flag did not select the fixed enable script")
	}
	if script, ok := privilegedScriptForArgs([]string{windowsHelperRemoveFlag}); !ok || script != windowsFirewallRemoveScript {
		t.Fatal("remove helper flag did not select the fixed removal script")
	}
	for _, args := range [][]string{
		nil,
		{"--openrung-direct-setup-enable-firewall-v1", `C:\untrusted.exe`},
		{"--arbitrary-command"},
	} {
		if _, ok := privilegedScriptForArgs(args); ok {
			t.Fatalf("unsafe helper argv accepted: %#v", args)
		}
	}
	if handled, _ := handlePrivilegedCommand([]string{windowsHelperEnableFlag}); handled {
		t.Fatal("portable application executable still accepts an elevated helper mode")
	}
}

func TestWindowsElevationUsesSystemPowerShellAndEncodedPathData(t *testing.T) {
	path := `C:\Users\Volunteer & Co\OpenRungVolunteer.exe`
	script := windowsPrivilegedFirewallScript(windowsFirewallEnableScript, path)
	if strings.Contains(script, path) {
		t.Fatal("untrusted executable path was inserted into PowerShell source")
	}
	if want := base64.StdEncoding.EncodeToString([]byte(path)); !strings.Contains(script, want) {
		t.Fatal("encoded executable data is missing")
	}
	requiredLauncherParts := []string{
		"$PSModuleAutoLoadingPreference = 'None'",
		"$env:PSModulePath = ''",
		"$PSHOME, 'powershell.exe'",
		"$startInfo.Verb = 'runas'",
		"OPENRUNG_ELEVATED_SCRIPT",
	}
	for _, required := range requiredLauncherParts {
		if !strings.Contains(windowsElevationLauncherScript, required) {
			t.Fatalf("elevation launcher is missing %q", required)
		}
	}
	if strings.Contains(windowsElevationLauncherScript, "OpenRungVolunteer.exe") {
		t.Fatal("portable application appears as the elevated executable")
	}
}

func TestWindowsRuleValidationAndMutationAreNarrow(t *testing.T) {
	requiredStatusChecks := []string{
		"OpenRungVolunteer-Direct-TCP-443-v1",
		"$PSModuleAutoLoadingPreference = 'None'",
		"$env:PSModulePath = ''",
		"Modules\\NetSecurity\\NetSecurity.psd1",
		"NetSecurity\\Get-NetFirewallRule",
		"$rule.Enabled",
		"$rule.Direction",
		"$rule.Action",
		"$rule.EdgeTraversalPolicy",
		"$applications[0].Program",
		"$ports[0].Protocol",
		"$ports[0].LocalPort",
	}
	for _, required := range requiredStatusChecks {
		if !strings.Contains(windowsFirewallStatusScript, required) {
			t.Fatalf("status script is missing exact validation %q", required)
		}
	}
	requiredSetupScopes := []string{
		"NetSecurity\\Remove-NetFirewallRule",
		"NetSecurity\\New-NetFirewallRule",
		"Direction = 'Inbound'",
		"Action = 'Allow'",
		"EdgeTraversalPolicy = 'Block'",
		"Program = $program",
		"Protocol = 'TCP'",
		"LocalPort = 443",
	}
	for _, required := range requiredSetupScopes {
		if !strings.Contains(windowsFirewallEnableScript, required) {
			t.Fatalf("enable script is missing narrow scope %q", required)
		}
	}
	if strings.Contains(windowsFirewallEnableScript, "netsh") ||
		strings.Contains(windowsFirewallEnableScript, "Any Any") {
		t.Fatal("enable script contains a broad/shell firewall fallback")
	}
}
