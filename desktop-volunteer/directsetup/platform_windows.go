//go:build windows

package directsetup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	windowsFirewallRuleName        = "OpenRungVolunteer-Direct-TCP-443-v1"
	windowsFirewallRuleDisplayName = "OpenRung Volunteer — Direct TCP 443"
	windowsFirewallRuleGroup       = "OpenRung Volunteer"
	windowsExecutableEnv           = "OPENRUNG_DIRECT_EXECUTABLE"
	windowsElevatedScriptEnv       = "OPENRUNG_ELEVATED_SCRIPT"

	windowsHelperEnableFlag = "--openrung-direct-setup-enable-firewall-v1"
	windowsHelperRemoveFlag = "--openrung-direct-setup-remove-firewall-v1"

	windowsElevationCancelledExit = 40
)

type windowsPlatform struct {
	executable    string
	powershell    string
	environment   []string
	runner        CommandRunner
	runElevated   func(context.Context, string) error
	statusCommand string
}

func newPlatform() Platform {
	executable, err := windowsExecutable()
	if err != nil {
		return staticPlatform{inspection: Inspection{
			Reason:  ReasonInspectionFailed,
			Message: "The OpenRung Volunteer executable path could not be resolved: " + err.Error(),
		}}
	}
	powershell, err := windowsPowerShellPath()
	if err != nil {
		return staticPlatform{inspection: Inspection{
			Reason:  ReasonToolUnavailable,
			Message: "Windows PowerShell could not be resolved from the Windows system directory: " + err.Error(),
		}}
	}
	environment, err := windowsCleanEnvironment(executable)
	if err != nil {
		return staticPlatform{inspection: Inspection{
			Reason:  ReasonInspectionFailed,
			Message: "A safe Windows helper environment could not be constructed: " + err.Error(),
		}}
	}
	return &windowsPlatform{
		executable:    executable,
		powershell:    powershell,
		environment:   environment,
		runner:        execCommandRunner{},
		runElevated:   runWindowsFirewallElevated,
		statusCommand: encodePowerShell(windowsFirewallStatusScript),
	}
}

func (p *windowsPlatform) Inspect(ctx context.Context) (Inspection, error) {
	output, err := p.runner.Run(ctx, Command{
		Path: p.powershell,
		Args: []string{
			"-NoLogo",
			"-NoProfile",
			"-NonInteractive",
			"-EncodedCommand", p.statusCommand,
		},
		Env: p.environment,
	})
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect Windows Defender Firewall rule: %w%s", err, conciseOutput(output))
	}

	switch strings.TrimSpace(string(output)) {
	case "READY":
		return Inspection{
			Ready:      true,
			Configured: true,
			Reason:     ReasonReady,
			Message: "Windows Defender Firewall has the exact application-scoped inbound TCP 443 rule. " +
				"This does not open a router/NAT mapping or cloud firewall. In Automatic mode, RelayHub's external probe must still succeed.",
		}, nil
	case "MISSING":
		return Inspection{
			Available: true,
			Reason:    ReasonFirewallRuleMissing,
			Message: "The application-scoped Windows Defender Firewall rule for inbound TCP 443 is missing. " +
				"Enabling direct connections will show one UAC prompt.",
		}, nil
	case "MISMATCH":
		return Inspection{
			Available:  true,
			Configured: true,
			Reason:     ReasonFirewallRuleMismatch,
			Message: "The OpenRung TCP 443 firewall rule is stale, duplicated, disabled, or points to a different executable. " +
				"Enabling direct connections will replace only that OpenRung rule with an exact application-scoped rule.",
		}, nil
	default:
		return Inspection{}, fmt.Errorf("unexpected firewall inspection result %q", strings.TrimSpace(string(output)))
	}
}

func (p *windowsPlatform) Enable(ctx context.Context) error {
	err := p.runElevated(ctx, windowsHelperEnableFlag)
	return classifyWindowsElevationError(err, "install the application-scoped Windows Defender Firewall rule")
}

func (p *windowsPlatform) Remove(ctx context.Context) error {
	err := p.runElevated(ctx, windowsHelperRemoveFlag)
	return classifyWindowsElevationError(err, "remove the OpenRung Windows Defender Firewall rule")
}

func classifyWindowsElevationError(err error, action string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_CANCELLED) {
		return &OperationError{
			Reason:  ReasonAuthorizationDeclined,
			Message: "The UAC prompt was cancelled. In Automatic mode, volunteering can continue on a distinct alternate port or through RelayHub.",
			Err:     err,
		}
	}
	if windowsCommandExitCode(err) == windowsElevationCancelledExit {
		return &OperationError{
			Reason:  ReasonAuthorizationDeclined,
			Message: "The UAC prompt was cancelled. In Automatic mode, volunteering can continue on a distinct alternate port or through RelayHub.",
			Err:     err,
		}
	}
	return &OperationError{
		Reason:  ReasonSetupFailed,
		Message: "Could not " + action + ".",
		Err:     err,
	}
}

// Windows elevates the signed system PowerShell executable, not the portable
// Wails application. There is therefore no privileged application-helper mode.
func handlePrivilegedCommand([]string) (bool, int) { return false, 0 }

func runWindowsFirewallElevated(ctx context.Context, operation string) error {
	script, ok := privilegedScriptForArgs([]string{operation})
	if !ok {
		return errors.New("unknown Windows firewall operation")
	}

	executable, err := windowsExecutable()
	if err != nil {
		return err
	}
	powershell, err := windowsPowerShellPath()
	if err != nil {
		return err
	}
	environment, err := windowsCleanEnvironment(executable)
	if err != nil {
		return err
	}
	// Only base64 data is inserted into the elevated script. The untrusted
	// executable path is decoded back into an environment data value inside
	// PowerShell and is never parsed as source or a command-line fragment.
	elevatedScript := windowsPrivilegedFirewallScript(script, executable)
	environment = append(environment, windowsElevatedScriptEnv+"="+encodePowerShell(elevatedScript))
	output, err := (execCommandRunner{}).Run(ctx, Command{
		Path: powershell,
		Args: []string{
			"-NoLogo",
			"-NoProfile",
			"-NonInteractive",
			"-EncodedCommand", encodePowerShell(windowsElevationLauncherScript),
		},
		Env: environment,
	})
	if err != nil {
		return fmt.Errorf("run Windows firewall authorization launcher: %w%s", err, conciseOutput(output))
	}
	return nil
}

func privilegedScriptForArgs(args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case windowsHelperEnableFlag:
		return windowsFirewallEnableScript, true
	case windowsHelperRemoveFlag:
		return windowsFirewallRemoveScript, true
	default:
		return "", false
	}
}

func windowsExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("executable is not a regular file")
	}
	return path, nil
}

func windowsPowerShellPath() (string, error) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	path := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("powershell.exe is not a regular file")
	}
	return path, nil
}

// windowsCleanEnvironment deliberately excludes caller-controlled PowerShell
// variables such as PSModulePath. The scripts disable module autoload, import
// NetSecurity from the fixed system PowerShell directory, and invoke its
// cmdlets with module-qualified names.
func windowsCleanEnvironment(executable string) ([]string, error) {
	windowsDirectory, err := windows.GetWindowsDirectory()
	if err != nil {
		return nil, err
	}
	windowsDirectory = filepath.Clean(windowsDirectory)
	return []string{
		"SystemRoot=" + windowsDirectory,
		"WINDIR=" + windowsDirectory,
		"ComSpec=" + filepath.Join(windowsDirectory, "System32", "cmd.exe"),
		windowsExecutableEnv + "=" + executable,
	}, nil
}

func windowsPrivilegedFirewallScript(script, executable string) string {
	pathData := base64.StdEncoding.EncodeToString([]byte(executable))
	return "$encodedProgram = '" + pathData + "'\n" +
		"$env:" + windowsExecutableEnv + " = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedProgram))\n" +
		script
}

func windowsCommandExitCode(err error) int {
	type exitCoder interface {
		ExitCode() int
	}
	var exitErr exitCoder
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, 0, len(encoded)*2)
	for _, value := range encoded {
		bytes = append(bytes, byte(value), byte(value>>8))
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

// This first stage runs unprivileged under the system PowerShell executable
// with a complete minimal environment. It asks UAC to start that same system
// executable for the already-encoded fixed firewall script. The portable
// OpenRung GUI image is never the elevated executable.
const windowsElevationLauncherScript = `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$PSModuleAutoLoadingPreference = 'None'
$env:PSModulePath = ''
$encoded = [Environment]::GetEnvironmentVariable('OPENRUNG_ELEVATED_SCRIPT')
if ([String]::IsNullOrWhiteSpace($encoded) -or $encoded -notmatch '^[A-Za-z0-9+/=]+$') {
    exit 41
}
$powerShell = [IO.Path]::Combine($PSHOME, 'powershell.exe')
$startInfo = [Diagnostics.ProcessStartInfo]::new()
$startInfo.FileName = $powerShell
$startInfo.Arguments = '-NoLogo -NoProfile -NonInteractive -EncodedCommand ' + $encoded
$startInfo.UseShellExecute = $true
$startInfo.Verb = 'runas'
$startInfo.WindowStyle = [Diagnostics.ProcessWindowStyle]::Hidden
try {
    $child = [Diagnostics.Process]::Start($startInfo)
} catch [ComponentModel.Win32Exception] {
    if ($_.Exception.NativeErrorCode -eq 1223) {
        exit 40
    }
    exit 41
}
if ($null -eq $child) {
    exit 41
}
$child.WaitForExit()
exit $child.ExitCode
`

const windowsFirewallScriptPrelude = `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$PSModuleAutoLoadingPreference = 'None'
$env:PSModulePath = ''
$netSecurityManifest = [IO.Path]::GetFullPath((Join-Path $PSHOME 'Modules\NetSecurity\NetSecurity.psd1'))
if (-not [IO.File]::Exists($netSecurityManifest)) {
    throw 'The system NetSecurity module is unavailable'
}
Microsoft.PowerShell.Core\Import-Module -Name $netSecurityManifest -Force -ErrorAction Stop
`

const windowsFirewallStatusScript = windowsFirewallScriptPrelude + `
$ruleName = 'OpenRungVolunteer-Direct-TCP-443-v1'
$expectedProgram = [IO.Path]::GetFullPath([Environment]::GetEnvironmentVariable('OPENRUNG_DIRECT_EXECUTABLE'))
$rules = @(NetSecurity\Get-NetFirewallRule -PolicyStore PersistentStore -Name $ruleName -ErrorAction SilentlyContinue)
if ($rules.Count -eq 0) {
    [Console]::Out.Write('MISSING')
    exit 0
}
if ($rules.Count -ne 1) {
    [Console]::Out.Write('MISMATCH')
    exit 0
}
$rule = $rules[0]
$applications = @($rule | NetSecurity\Get-NetFirewallApplicationFilter)
$ports = @($rule | NetSecurity\Get-NetFirewallPortFilter)
$programMatches = $false
if ($applications.Count -eq 1 -and $applications[0].Program -and $applications[0].Program -ne 'Any') {
    $actualProgram = [IO.Path]::GetFullPath([string]$applications[0].Program)
    $programMatches = [StringComparer]::OrdinalIgnoreCase.Equals($actualProgram, $expectedProgram)
}
$valid = (
    [string]$rule.Enabled -eq 'True' -and
    [string]$rule.Direction -eq 'Inbound' -and
    [string]$rule.Action -eq 'Allow' -and
    [string]$rule.Profile -eq 'Any' -and
    [string]$rule.EdgeTraversalPolicy -eq 'Block' -and
    $programMatches -and
    $ports.Count -eq 1 -and
    [string]$ports[0].Protocol -eq 'TCP' -and
    [string]$ports[0].LocalPort -eq '443'
)
if ($valid) {
    [Console]::Out.Write('READY')
} else {
    [Console]::Out.Write('MISMATCH')
}
`

const windowsFirewallEnableScript = windowsFirewallScriptPrelude + `
$ruleName = 'OpenRungVolunteer-Direct-TCP-443-v1'
$program = [IO.Path]::GetFullPath([Environment]::GetEnvironmentVariable('OPENRUNG_DIRECT_EXECUTABLE'))
if (-not [IO.File]::Exists($program)) {
    throw 'OpenRung Volunteer executable does not exist'
}
NetSecurity\Get-NetFirewallRule -PolicyStore PersistentStore -Name $ruleName -ErrorAction SilentlyContinue |
    NetSecurity\Remove-NetFirewallRule -PolicyStore PersistentStore -ErrorAction Stop
$parameters = @{
    PolicyStore = 'PersistentStore'
    Name = $ruleName
    DisplayName = 'OpenRung Volunteer — Direct TCP 443'
    Group = 'OpenRung Volunteer'
    Enabled = 'True'
    Direction = 'Inbound'
    Action = 'Allow'
    Profile = 'Any'
    EdgeTraversalPolicy = 'Block'
    Program = $program
    Protocol = 'TCP'
    LocalPort = 443
}
NetSecurity\New-NetFirewallRule @parameters | Out-Null
`

const windowsFirewallRemoveScript = windowsFirewallScriptPrelude + `
$ruleName = 'OpenRungVolunteer-Direct-TCP-443-v1'
NetSecurity\Get-NetFirewallRule -PolicyStore PersistentStore -Name $ruleName -ErrorAction SilentlyContinue |
    NetSecurity\Remove-NetFirewallRule -PolicyStore PersistentStore -ErrorAction Stop
`
