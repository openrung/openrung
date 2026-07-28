//go:build linux

package directsetup

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

const (
	linuxHelperEnableFlag = "--openrung-direct-setup-enable-capability-v1"
	linuxHelperRemoveFlag = "--openrung-direct-setup-remove-capability-v1"

	// Fixed helper exit codes carry only a classification across the pkexec
	// boundary. Raw privileged command output is never reflected into the GUI.
	linuxHelperExitSetupFailed           = 20
	linuxHelperExitFilesystemUnsupported = 21
	linuxHelperExitCapabilityMismatch    = 22

	// Authorization has already completed before this helper starts. Bound
	// the fixed getcap/setcap work independently so a wedged privileged child
	// cannot outlive the GUI operation indefinitely.
	linuxPrivilegedOperationTimeout = 30 * time.Second
)

type linuxPrivilegedOperation int

const (
	linuxPrivilegedEnable linuxPrivilegedOperation = iota + 1
	linuxPrivilegedRemove
)

// handlePrivilegedCommand runs before Wails is initialized. pkexec may launch
// this exact, root-owned application binary with one fixed flag; the process
// performs only the capability operation and exits, so no root GUI starts.
func handlePrivilegedCommand(args []string) (bool, int) {
	operation, ok := linuxPrivilegedOperationForArgs(args)
	if !ok {
		return false, 0
	}
	if os.Geteuid() != 0 {
		return true, linuxHelperExitSetupFailed
	}

	executable, err := canonicalExecutable()
	if err != nil {
		return true, linuxHelperExitSetupFailed
	}
	if err := linuxSecureInstalledExecutable(executable); err != nil {
		return true, linuxHelperExitSetupFailed
	}
	getcapPath := firstExecutable("/usr/sbin/getcap", "/sbin/getcap", "/usr/bin/getcap", "/bin/getcap")
	setcapPath := firstExecutable("/usr/sbin/setcap", "/sbin/setcap", "/usr/bin/setcap", "/bin/setcap")
	if getcapPath == "" || setcapPath == "" {
		return true, linuxHelperExitSetupFailed
	}
	ctx, cancel := context.WithTimeout(context.Background(), linuxPrivilegedOperationTimeout)
	defer cancel()
	if err := runLinuxPrivilegedOperation(
		ctx,
		operation,
		executable,
		getcapPath,
		setcapPath,
		execCommandRunner{},
	); err != nil {
		return true, linuxHelperExitCode(err)
	}
	return true, 0
}

func linuxPrivilegedOperationForArgs(args []string) (linuxPrivilegedOperation, bool) {
	if len(args) != 1 {
		return 0, false
	}
	switch args[0] {
	case linuxHelperEnableFlag:
		return linuxPrivilegedEnable, true
	case linuxHelperRemoveFlag:
		return linuxPrivilegedRemove, true
	default:
		return 0, false
	}
}

// runLinuxPrivilegedOperation rechecks the current file capability inside the
// authorized process, immediately before mutation. This prevents Enable from
// overwriting, or Remove from clearing, unexpected administrator-managed
// capabilities that appeared while the polkit prompt was open.
func runLinuxPrivilegedOperation(
	ctx context.Context,
	operation linuxPrivilegedOperation,
	executable, getcapPath, setcapPath string,
	runner CommandRunner,
) error {
	output, err := runner.Run(ctx, Command{
		Path: getcapPath,
		Args: []string{executable},
	})
	if err != nil {
		return err
	}
	capability := capabilityField(string(output))

	switch operation {
	case linuxPrivilegedEnable:
		if capability == linuxBindCapability {
			return nil
		}
		if capability != "" {
			return errors.New("refusing to replace unexpected file capabilities")
		}
		output, err = runner.Run(ctx, Command{
			Path: setcapPath,
			Args: []string{linuxBindCapability, executable},
		})
		if err != nil {
			return &linuxHelperOperationError{Err: err, Detail: string(output)}
		}
		return nil
	case linuxPrivilegedRemove:
		if capability == "" {
			return nil
		}
		if capability != linuxBindCapability {
			return errors.New("refusing to remove unexpected file capabilities")
		}
		output, err = runner.Run(ctx, Command{
			Path: setcapPath,
			Args: []string{"-r", executable},
		})
		if err != nil {
			return &linuxHelperOperationError{Err: err, Detail: string(output)}
		}
		return nil
	default:
		return errors.New("unknown privileged capability operation")
	}
}

type linuxHelperOperationError struct {
	Err    error
	Detail string
}

func (e *linuxHelperOperationError) Error() string { return e.Err.Error() }
func (e *linuxHelperOperationError) Unwrap() error { return e.Err }

func linuxHelperExitCode(err error) int {
	detail := strings.ToLower(err.Error())
	var operationError *linuxHelperOperationError
	if errors.As(err, &operationError) {
		detail += " " + strings.ToLower(operationError.Detail)
	}
	switch {
	case strings.Contains(detail, "operation not supported"),
		strings.Contains(detail, "not supported by filesystem"):
		return linuxHelperExitFilesystemUnsupported
	case strings.Contains(detail, "unexpected file capabilities"):
		return linuxHelperExitCapabilityMismatch
	default:
		return linuxHelperExitSetupFailed
	}
}
