// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// RunDir holds pidfiles, generated configuration and per-service state.
	RunDir = "/run/lab"
	// LogDir holds one log file per service.
	LogDir = "/var/log/lab"
)

// Service is a process the lab runs inside a node.
type Service struct {
	Name string
	Node string
	Argv []string
	Env  []string // KEY=value, added to a minimal base environment
}

// Start launches a service detached from the caller, so it outlives a
// short-lived `lab` command. Output goes to LogPath(name).
func Start(s Service) error {
	if Running(s.Name) {
		return fmt.Errorf("service %s is already running", s.Name)
	}
	for _, dir := range []string{filepath.Join(RunDir, "pids"), LogDir, StateDir(s.Name)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	logFile, err := os.OpenFile(LogPath(s.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "--- lab: start %s in %s: %s\n", s.Name, s.Node, strings.Join(s.Argv, " "))

	cmd := exec.Command("ip", append([]string{"netns", "exec", s.Node}, s.Argv...)...)
	cmd.Env = append([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + StateDir(s.Name),
		"TMPDIR=" + StateDir(s.Name),
	}, s.Env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if Trace != nil {
		Trace("start " + s.Name + ": " + strings.Join(cmd.Args, " "))
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.Name, err)
	}
	// Reap the child if this process outlives it; otherwise init does.
	go func() { _ = cmd.Wait() }()
	return os.WriteFile(pidPath(s.Name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
}

// Stop terminates a service and everything it started, politely first.
func Stop(name string) error { return signalAndWait(name, syscall.SIGTERM, 5*time.Second) }

// Kill ends a service abruptly, like a crash or a power cut.
func Kill(name string) error { return signalAndWait(name, syscall.SIGKILL, 0) }

func signalAndWait(name string, sig syscall.Signal, grace time.Duration) error {
	pid, ok := servicePID(name)
	if !ok {
		return nil
	}
	defer os.Remove(pidPath(name))
	// The service leads its own session, so its negative pid addresses its
	// process group. Children that moved to a group of their own (a relay's
	// Xray, the client's tunnel runtime) stay in the session, so the wait
	// and the final kill cover the session.
	_ = syscall.Kill(-pid, sig)
	deadline := time.Now().Add(grace)
	for len(sessionPIDs(pid)) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	for _, member := range sessionPIDs(pid) {
		_ = syscall.Kill(member, syscall.SIGKILL)
	}
	return Eventually(5*time.Second, func() error {
		if left := sessionPIDs(pid); len(left) > 0 {
			return fmt.Errorf("service %s still has processes %v", name, left)
		}
		return nil
	})
}

// Running reports whether a service's process is alive.
func Running(name string) bool {
	pid, ok := servicePID(name)
	return ok && processAlive(pid)
}

// RunningServices lists the services that are alive, sorted.
func RunningServices() []string {
	entries, _ := os.ReadDir(filepath.Join(RunDir, "pids"))
	var names []string
	for _, entry := range entries {
		if Running(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// StopAll stops every running service.
func StopAll() {
	for _, name := range RunningServices() {
		_ = Kill(name)
	}
}

// LogPath is where a service's output is written.
func LogPath(name string) string { return filepath.Join(LogDir, name+".log") }

// StateDir is a service's private scratch directory (also its HOME).
func StateDir(name string) string { return filepath.Join(RunDir, "state", name) }

// ReadLog returns a service's log from a byte offset onwards.
func ReadLog(name string, from int64) (string, error) {
	data, err := os.ReadFile(LogPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if from > int64(len(data)) {
		from = int64(len(data))
	}
	return string(data[from:]), nil
}

// LogSize returns the current length of a service's log, for use as the
// offset of a later ReadLog or WaitLog.
func LogSize(name string) int64 {
	info, err := os.Stat(LogPath(name))
	if err != nil {
		return 0
	}
	return info.Size()
}

// WaitLog waits until a service's log contains text after the given offset.
func WaitLog(name string, from int64, text string, timeout time.Duration) error {
	return Eventually(timeout, func() error {
		log, err := ReadLog(name, from)
		if err != nil {
			return err
		}
		if !strings.Contains(log, text) {
			return fmt.Errorf("log of %s does not contain %q yet", name, text)
		}
		return nil
	})
}

func pidPath(name string) string { return filepath.Join(RunDir, "pids", name) }

func servicePID(name string) (int, bool) {
	data, err := os.ReadFile(pidPath(name))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid, err == nil && pid > 0
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state letter follows the parenthesised command name.
	rest := stat[bytes.LastIndexByte(stat, ')')+1:]
	fields := bytes.Fields(rest)
	return len(fields) > 0 && string(fields[0]) != "Z"
}

// sessionPIDs returns the live processes in the session led by sid.
func sessionPIDs(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// After the command name: state, ppid, pgrp, session.
		fields := bytes.Fields(stat[bytes.LastIndexByte(stat, ')')+1:])
		if len(fields) < 4 || string(fields[0]) == "Z" {
			continue
		}
		if session, _ := strconv.Atoi(string(fields[3])); session == sid {
			pids = append(pids, pid)
		}
	}
	return pids
}
