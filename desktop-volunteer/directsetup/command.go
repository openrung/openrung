package directsetup

import (
	"context"
	"os/exec"
)

// Command describes a process invocation without a shell. Args are passed as
// individual argv values. A nil Env inherits the current environment; a
// non-nil Env is the complete environment for the child. Privileged command
// paths use a deliberately minimal non-nil environment.
type Command struct {
	Path string
	Args []string
	Env  []string
}

// CommandRunner is injectable so tests never touch firewall rules, file
// capabilities, UAC, or polkit.
type CommandRunner interface {
	Run(context.Context, Command) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, spec Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	configureCommand(cmd)
	if spec.Env != nil {
		cmd.Env = append([]string(nil), spec.Env...)
	}
	return cmd.CombinedOutput()
}
