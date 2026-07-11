//go:build windows

package client

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureSingBoxProcess prevents the console-subsystem sing-box.exe from
// creating a blank Command Prompt window when launched by the desktop GUI.
func configureSingBoxProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

func interruptSingBoxProcess(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}

func killSingBoxProcess(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
