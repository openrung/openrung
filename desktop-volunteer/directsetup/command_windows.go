//go:build windows

package directsetup

import (
	"os/exec"
	"syscall"
)

// PowerShell is an implementation detail of setup/status and must not flash a
// console window next to the Wails GUI or privileged helper.
func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
