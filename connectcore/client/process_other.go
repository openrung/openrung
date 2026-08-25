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

// superviseSingBoxProcess is the Windows kill-on-close job backstop; off
// Windows there is no kernel object that reaps the child if this process dies
// without running teardown — the stdin-close stop pipe is what covers that
// (the OS closes it on parent death, and the bundled child stops itself).
func superviseSingBoxProcess(*exec.Cmd) (release func()) {
	return func() {}
}

func interruptSingBoxProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}

func killSingBoxProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
