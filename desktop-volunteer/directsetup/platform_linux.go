//go:build linux

package directsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	linuxLowPortStartPath  = "/proc/sys/net/ipv4/ip_unprivileged_port_start"
	linuxProcessStatusPath = "/proc/self/status"
	linuxBindCapability    = "cap_net_bind_service=ep"
)

type fileReader interface {
	ReadFile(string) ([]byte, error)
}

type osFileReader struct{}

func (osFileReader) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

type linuxPlatform struct {
	executable string
	getcapPath string
	setcapPath string
	pkexecPath string
	runner     CommandRunner
	files      fileReader
	firewall   string
	securePath func(string) error

	mu                    sync.Mutex
	unavailableReason     string
	unavailableMessage    string
	capabilityJustEnabled bool
	capabilityJustRemoved bool
}

func newPlatform() Platform {
	executable, err := canonicalExecutable()
	if err != nil {
		return staticPlatform{inspection: Inspection{
			Reason:  ReasonInspectionFailed,
			Message: "The OpenRung Volunteer executable path could not be resolved: " + err.Error(),
		}}
	}
	return &linuxPlatform{
		executable: executable,
		getcapPath: firstExecutable("/usr/sbin/getcap", "/sbin/getcap", "/usr/bin/getcap", "/bin/getcap"),
		setcapPath: firstExecutable("/usr/sbin/setcap", "/sbin/setcap", "/usr/bin/setcap", "/bin/setcap"),
		pkexecPath: firstExecutable("/usr/bin/pkexec", "/bin/pkexec"),
		runner:     execCommandRunner{},
		files:      osFileReader{},
		firewall:   linuxFirewallGuidance(),
		securePath: linuxSecureInstalledExecutable,
	}
}

func (p *linuxPlatform) Inspect(ctx context.Context) (Inspection, error) {
	p.mu.Lock()
	unavailableReason := p.unavailableReason
	unavailableMessage := p.unavailableMessage
	capabilityJustEnabled := p.capabilityJustEnabled
	capabilityJustRemoved := p.capabilityJustRemoved
	p.mu.Unlock()
	if unavailableReason != "" {
		return Inspection{Reason: unavailableReason, Message: unavailableMessage}, nil
	}
	if capabilityJustRemoved {
		return Inspection{
			Reason: ReasonRemovalRestartRequired,
			Message: "The file capability was removed, but this running process retains its already-loaded permission. " +
				"Quit and reopen OpenRung Volunteer to complete removal; volunteering cannot restart in this process.",
		}, nil
	}

	if p.getcapPath == "" {
		if lowPortsUnprivileged(p.files) {
			return Inspection{
				Ready:  true,
				Reason: ReasonLowPortsUnprivileged,
				Message: "This Linux host permits unprivileged processes to bind TCP 443; no file capability is needed. " +
					"getcap is unavailable, so OpenRung cannot inspect whether an obsolete file capability remains." + p.firewall,
			}, nil
		}
		return Inspection{
			Reason:  ReasonToolUnavailable,
			Message: "The getcap tool from libcap is unavailable, so OpenRung cannot safely inspect this binary's TCP 443 permission.",
		}, nil
	}

	capability, err := p.readCapability(ctx)
	if err != nil {
		return Inspection{}, err
	}

	toolsAvailable := p.setcapPath != "" && p.pkexecPath != ""
	secureErr := p.checkSecurePath()

	if lowPortsUnprivileged(p.files) {
		switch capability {
		case "":
			return Inspection{
				Ready:  true,
				Reason: ReasonLowPortsUnprivileged,
				Message: "This Linux host permits unprivileged processes to bind TCP 443; no file capability is needed." +
					p.firewall,
			}, nil
		case linuxBindCapability:
			message := "This Linux host permits unprivileged TCP 443, so the existing CAP_NET_BIND_SERVICE file capability is unnecessary."
			if !toolsAvailable {
				message += " Install libcap and polkit before removing the obsolete capability."
			} else if secureErr != nil {
				message += " OpenRung will not elevate against this user-writable portable path; ask an administrator to inspect and remove the capability."
			} else {
				message += " Remove setup is available and will remove only this exact OpenRung capability."
			}
			return Inspection{
				Ready:      true,
				Configured: toolsAvailable && secureErr == nil,
				Reason:     ReasonLowPortsUnprivileged,
				Message:    message + p.firewall,
			}, nil
		default:
			return Inspection{
				Ready:  true,
				Reason: ReasonCapabilityMismatch,
				Message: "This Linux host permits unprivileged TCP 443, but this binary has unexpected file capabilities. " +
					"OpenRung will not replace or remove administrator-managed capabilities; ask an administrator to review them." + p.firewall,
			}, nil
		}
	}

	switch {
	case capability == "":
		if secureErr != nil {
			return Inspection{
				Reason: ReasonInsecureExecutable,
				Message: "Least-privilege TCP 443 setup is available only when the installed OpenRung Volunteer binary " +
					"and every ancestor directory are root-owned and not group- or world-writable. This portable/user-writable copy is not eligible: " + secureErr.Error(),
			}, nil
		}
		if !toolsAvailable {
			return Inspection{
				Reason: ReasonToolUnavailable,
				Message: "TCP 443 needs CAP_NET_BIND_SERVICE, but setcap or pkexec is unavailable. " +
					"Install libcap and polkit through your operating system, then retry.",
			}, nil
		}
		return Inspection{
			Available: true,
			Reason:    ReasonCapabilityMissing,
			Message: "This OpenRung Volunteer binary does not have CAP_NET_BIND_SERVICE. Enabling direct connections " +
				"will request authorization once and grant only that file capability." + p.firewall,
		}, nil
	case capability != linuxBindCapability:
		return Inspection{
			Reason: ReasonCapabilityMismatch,
			Message: "This binary has file capabilities other than the exact CAP_NET_BIND_SERVICE permission. " +
				"OpenRung will not replace or remove administrator-managed capabilities; ask an administrator to review them.",
		}, nil
	default:
		if !processHasBindCapability(p.files) {
			if !capabilityJustEnabled {
				message := "CAP_NET_BIND_SERVICE is present on this exact binary but was not effective in this newly started process. " +
					"A nosuid mount, no-new-privileges policy, filesystem, or security policy may be suppressing file capabilities."
				if toolsAvailable && secureErr == nil {
					message += " Remove setup is available; otherwise ask an administrator to correct the installation policy."
				}
				return Inspection{
					Configured: toolsAvailable && secureErr == nil,
					Reason:     ReasonCapabilityIneffective,
					Message:    message,
				}, nil
			}
			return Inspection{
				Available:  true,
				Configured: toolsAvailable && secureErr == nil,
				Reason:     ReasonRestartRequired,
				Message: "CAP_NET_BIND_SERVICE is installed on this exact binary, but file capabilities only apply to " +
					"a newly started process. Quit and reopen OpenRung Volunteer before TCP 443 can be used." + p.firewall,
			}, nil
		}
		message := "This running OpenRung Volunteer process has CAP_NET_BIND_SERVICE for TCP 443."
		if !toolsAvailable {
			message += " setcap or pkexec is unavailable, so OpenRung cannot offer removal."
		} else if secureErr != nil {
			message += " The executable path is not a secure root-owned installation, so OpenRung will not elevate to remove it."
		}
		return Inspection{
			Ready:      true,
			Configured: toolsAvailable && secureErr == nil,
			Reason:     ReasonReady,
			Message:    message + p.firewall,
		}, nil
	}
}

func (p *linuxPlatform) Enable(ctx context.Context) error {
	if p.setcapPath == "" || p.pkexecPath == "" {
		return &OperationError{
			Reason:      ReasonToolUnavailable,
			Unavailable: true,
			Message:     "setcap and pkexec are required for least-privilege TCP 443 setup.",
		}
	}
	if lowPortsUnprivileged(p.files) {
		return nil
	}
	capability, err := p.readCapability(ctx)
	if err != nil {
		return &OperationError{Reason: ReasonInspectionFailed, Message: err.Error(), Err: err}
	}
	if capability == linuxBindCapability {
		return nil
	}
	if capability != "" {
		return &OperationError{
			Reason:      ReasonCapabilityMismatch,
			Unavailable: true,
			Message:     "OpenRung will not replace unexpected administrator-managed file capabilities.",
		}
	}
	if err := p.checkSecurePath(); err != nil {
		return &OperationError{
			Reason:      ReasonInsecureExecutable,
			Unavailable: true,
			Message:     "OpenRung will not grant a capability to a user-writable portable executable: " + err.Error(),
			Err:         err,
		}
	}

	output, err := p.runner.Run(ctx, Command{
		Path: p.pkexecPath,
		Args: []string{p.executable, linuxHelperEnableFlag},
	})
	if err == nil {
		p.mu.Lock()
		p.capabilityJustEnabled = true
		p.mu.Unlock()
		return nil
	}
	return p.classifyAuthorizationError(err, output, "grant CAP_NET_BIND_SERVICE")
}

func (p *linuxPlatform) Remove(ctx context.Context) error {
	if p.setcapPath == "" || p.pkexecPath == "" {
		return &OperationError{
			Reason:      ReasonToolUnavailable,
			Unavailable: true,
			Message:     "setcap and pkexec are required to remove the TCP 443 file capability.",
		}
	}
	capability, err := p.readCapability(ctx)
	if err != nil {
		return &OperationError{Reason: ReasonInspectionFailed, Message: err.Error(), Err: err}
	}
	if capability == "" {
		return nil
	}
	if capability != linuxBindCapability {
		return &OperationError{
			Reason:      ReasonCapabilityMismatch,
			Unavailable: true,
			Message:     "OpenRung will not remove unexpected administrator-managed file capabilities.",
		}
	}
	if err := p.checkSecurePath(); err != nil {
		return &OperationError{
			Reason:      ReasonInsecureExecutable,
			Unavailable: true,
			Message:     "OpenRung will not elevate against a user-writable portable executable: " + err.Error(),
			Err:         err,
		}
	}
	output, err := p.runner.Run(ctx, Command{
		Path: p.pkexecPath,
		Args: []string{p.executable, linuxHelperRemoveFlag},
	})
	if err == nil {
		p.mu.Lock()
		p.capabilityJustEnabled = false
		p.capabilityJustRemoved = true
		p.mu.Unlock()
		return nil
	}
	return p.classifyAuthorizationError(err, output, "remove CAP_NET_BIND_SERVICE")
}

func (p *linuxPlatform) readCapability(ctx context.Context) (string, error) {
	output, err := p.runner.Run(ctx, Command{
		Path: p.getcapPath,
		Args: []string{p.executable},
	})
	if err != nil {
		return "", fmt.Errorf("inspect %s with getcap: %w%s", filepath.Base(p.executable), err, conciseOutput(output))
	}
	return capabilityField(string(output)), nil
}

func (p *linuxPlatform) checkSecurePath() error {
	if p.securePath == nil {
		return errors.New("secure executable-path inspection is unavailable")
	}
	return p.securePath(p.executable)
}

func (p *linuxPlatform) classifyAuthorizationError(err error, output []byte, action string) error {
	detail := strings.ToLower(string(output) + " " + err.Error())
	if strings.Contains(detail, "operation not supported") ||
		strings.Contains(detail, "not supported by filesystem") {
		message := "This filesystem does not support the file capability required for TCP 443. " +
			"Move/install OpenRung Volunteer on a capability-supporting local filesystem; the GUI will not run as root."
		p.mu.Lock()
		p.unavailableReason = ReasonFilesystemUnsupported
		p.unavailableMessage = message
		p.mu.Unlock()
		return &OperationError{
			Reason:      ReasonFilesystemUnsupported,
			Unavailable: true,
			Message:     message,
			Err:         err,
		}
	}

	switch commandExitCode(err) {
	case linuxHelperExitFilesystemUnsupported:
		message := "This filesystem does not support the file capability required for TCP 443. " +
			"Move/install OpenRung Volunteer on a capability-supporting local filesystem; the GUI will not run as root."
		p.mu.Lock()
		p.unavailableReason = ReasonFilesystemUnsupported
		p.unavailableMessage = message
		p.mu.Unlock()
		return &OperationError{
			Reason:      ReasonFilesystemUnsupported,
			Unavailable: true,
			Message:     message,
			Err:         err,
		}
	case linuxHelperExitCapabilityMismatch:
		return &OperationError{
			Reason:      ReasonCapabilityMismatch,
			Unavailable: true,
			Message:     "The authorized helper found unexpected administrator-managed capabilities and refused to change them.",
			Err:         err,
		}
	case 126:
		return &OperationError{
			Reason:  ReasonAuthorizationDeclined,
			Message: "Authorization was cancelled. In Automatic mode, volunteering can continue on a distinct alternate port or through RelayHub.",
			Err:     err,
		}
	case 127:
		return &OperationError{
			Reason:  ReasonAuthorizationDenied,
			Message: "Authorization was not granted. In Automatic mode, volunteering can continue on a distinct alternate port or through RelayHub.",
			Err:     err,
		}
	}
	if strings.Contains(detail, "not authorized") ||
		strings.Contains(detail, "authentication failed") {
		return &OperationError{
			Reason:  ReasonAuthorizationDenied,
			Message: "Authorization was not granted. In Automatic mode, volunteering can continue on a distinct alternate port or through RelayHub.",
			Err:     err,
		}
	}
	return &OperationError{
		Reason:  ReasonSetupFailed,
		Message: "Could not " + action + "." + conciseOutput(output),
		Err:     err,
	}
}

func lowPortsUnprivileged(files fileReader) bool {
	data, err := files.ReadFile(linuxLowPortStartPath)
	if err != nil {
		return false
	}
	first := strings.Fields(string(data))
	if len(first) == 0 {
		return false
	}
	start, err := strconv.Atoi(first[0])
	return err == nil && start <= DirectPort
}

func processHasBindCapability(files fileReader) bool {
	data, err := files.ReadFile(linuxProcessStatusPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		return err == nil && value&(uint64(1)<<10) != 0
	}
	return false
}

func capabilityField(output string) string {
	for _, field := range strings.Fields(strings.TrimSpace(output)) {
		if strings.HasPrefix(field, "cap_") {
			return strings.ToLower(field)
		}
	}
	return ""
}

func linuxFirewallGuidance() string {
	switch {
	case firstExecutable("/usr/sbin/ufw", "/usr/bin/ufw") != "":
		return " UFW tooling is installed; OpenRung does not change its rules. If Automatic's external probe fails, ask your administrator to review an explicit inbound TCP 443 rule."
	case firstExecutable("/usr/bin/firewall-cmd", "/bin/firewall-cmd") != "":
		return " firewalld tooling is installed; OpenRung does not change its zones or rules. If Automatic's external probe fails, ask your administrator to review an explicit inbound TCP 443 rule."
	case firstExecutable("/usr/sbin/nft", "/usr/bin/nft") != "":
		return " nftables tooling is installed; OpenRung does not change its ruleset. If Automatic's external probe fails, ask your administrator to review an explicit inbound TCP 443 rule."
	default:
		return " This setup does not change a host firewall, router/NAT mapping, or cloud security group; in Automatic mode, those may still block the external probe."
	}
}

func canonicalExecutable() (string, error) {
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
		return "", fmt.Errorf("resolve executable symlinks: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("executable is not a regular file")
	}
	return filepath.Clean(path), nil
}

// linuxSecureInstalledExecutable prevents a user-writable-path swap while a
// polkit prompt is open. setcap is only offered for a regular, root-owned,
// non-group/world-writable binary beneath root-owned, non-writable
// directories. Portable tarball copies remain fully usable on alternate ports
// and RelayHub but are not eligible for privileged mutation in place.
func linuxSecureInstalledExecutable(path string) error {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("executable is not a regular file")
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return errors.New("executable must not have setuid or setgid bits")
	}
	if err := requireRootOwnedNonWritable(path, info); err != nil {
		return err
	}

	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", directory)
		}
		if err := requireRootOwnedNonWritable(directory, info); err != nil {
			return err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	return nil
}

func requireRootOwnedNonWritable(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify owner of %s", path)
	}
	if stat.Uid != 0 {
		return fmt.Errorf("%s is not owned by root", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group- or world-writable", path)
	}
	return nil
}

func firstExecutable(paths ...string) string {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func commandExitCode(err error) int {
	type exitCoder interface {
		ExitCode() int
	}
	var exitErr exitCoder
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
