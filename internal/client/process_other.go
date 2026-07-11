//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// configureSingBoxProcess applies platform-specific process settings before
// sing-box starts. A separate process group lets cancellation tear down helper
// processes too; otherwise a child that inherits the listener can keep a ladder
// rung alive after the parent is killed.
func configureSingBoxProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptSingBoxProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}

func killSingBoxProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
