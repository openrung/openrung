//go:build windows

package client

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// configureSingBoxProcess prevents the console-subsystem sing-box.exe from
// creating a blank Command Prompt window when launched by the desktop GUI.
func configureSingBoxProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// superviseSingBoxProcess binds the started child to a kill-on-close job
// object whose only handle lives in this process: if this process dies without
// running its teardown — a crash, Stop-Process, Task Manager — the kernel
// closes the handle and ends the child, the Windows counterpart of the Unix
// process group's "no orphan outlives the runner". It is the LAST line: the
// job kill is a TerminateProcess with no cleanup, so the stdin-close stop
// channel (which lets an orphaned TUN child unwind its routes and DNS first)
// and the runner's kill ladder remain the teardown paths that matter.
// Best-effort by design — a hardened environment can refuse job APIs, and a
// missing backstop must not fail the tunnel.
//
// The returned release closes the job handle. Run defers it, so on the
// hard-kill-timeout path — Run giving up on a child that ignored even the kill
// — the close doubles as one more attempt to take the child down.
func superviseSingBoxProcess(cmd *exec.Cmd) (release func()) {
	noop := func() {}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return noop
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return noop
	}
	// os.Process keeps its own handle open until Wait, so the pid cannot be
	// recycled between Start and this OpenProcess.
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return noop
	}
	assignErr := windows.AssignProcessToJobObject(job, proc)
	_ = windows.CloseHandle(proc)
	if assignErr != nil {
		_ = windows.CloseHandle(job)
		return noop
	}
	return func() { _ = windows.CloseHandle(job) }
}

func interruptSingBoxProcess(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}

func killSingBoxProcess(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
