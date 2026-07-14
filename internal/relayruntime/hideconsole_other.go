//go:build !windows

package relayruntime

import "os/exec"

// ConfigureBackgroundCommand is a no-op outside Windows; see the windows build
// for why it exists.
func ConfigureBackgroundCommand(cmd *exec.Cmd) {}
