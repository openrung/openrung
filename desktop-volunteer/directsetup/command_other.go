//go:build !windows

package directsetup

import "os/exec"

func configureCommand(*exec.Cmd) {}
