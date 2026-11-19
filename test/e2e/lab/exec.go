// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Trace, when set, receives every command the lab runs. The CLI wires it to
// stderr under -v so a reader can see exactly which ip/tc/iptables calls build
// a scenario.
var Trace func(line string)

// run executes a command in the container's root namespace.
func run(argv ...string) error {
	_, err := output(argv...)
	return err
}

// output executes a command and returns its trimmed stdout.
func output(argv ...string) (string, error) {
	if Trace != nil {
		Trace(strings.Join(argv, " "))
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// In runs a command inside a node's network namespace.
func In(node string, argv ...string) error {
	_, err := OutputIn(node, argv...)
	return err
}

// OutputIn runs a command inside a node's network namespace and returns its
// trimmed stdout.
func OutputIn(node string, argv ...string) (string, error) {
	return output(append([]string{"ip", "netns", "exec", node}, argv...)...)
}

// OutputInContext is OutputIn with a deadline; the command is killed when ctx
// ends.
func OutputInContext(ctx context.Context, node string, argv ...string) (string, error) {
	full := append([]string{"netns", "exec", node}, argv...)
	if Trace != nil {
		Trace("ip " + strings.Join(full, " "))
	}
	cmd := exec.CommandContext(ctx, "ip", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s in %s: %w: %s", strings.Join(argv, " "), node, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Eventually polls check until it returns nil or the timeout passes, and
// returns the last error.
func Eventually(timeout time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
