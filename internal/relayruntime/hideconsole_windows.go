//go:build windows

package relayruntime

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// ConfigureBackgroundCommand prevents console-subsystem child processes (xray)
// from flashing a blank Command Prompt window when spawned by a GUI app.
func ConfigureBackgroundCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
