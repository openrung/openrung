//go:build !windows

package client

import "os/exec"

// configureSingBoxProcess applies platform-specific process settings before
// sing-box starts.
func configureSingBoxProcess(cmd *exec.Cmd) {}
