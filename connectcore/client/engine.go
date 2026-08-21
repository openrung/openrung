package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const singBoxHardKillWait = 2 * time.Second

type SingBoxRunner struct {
	Path   string
	Stdout io.Writer
	Stderr io.Writer
	// KillGrace bounds the wait between the interrupt sent on context cancel
	// and the hard kill. Zero keeps the 5s default. The desktop connect ladder
	// shortens it: os.Interrupt is unsupported on Windows, so without a short
	// grace every failed candidate's teardown would cost the full default.
	KillGrace time.Duration
}

func (r SingBoxRunner) Run(ctx context.Context, configPath string) error {
	if configPath == "" {
		return errors.New("sing-box config path is required")
	}

	binary := r.Path
	if binary == "" {
		binary = "sing-box"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("find sing-box %q: %w", binary, err)
	}

	cmd := exec.Command(resolved, "run", "-c", configPath)
	configureSingBoxProcess(cmd)
	cmd.Stdout = r.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = r.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		if err != nil {
			return fmt.Errorf("sing-box exited: %w", err)
		}
		return errors.New("sing-box exited")
	case <-ctx.Done():
		grace := r.KillGrace
		if grace <= 0 {
			grace = 5 * time.Second
		}
		_ = interruptSingBoxProcess(cmd)
		select {
		case <-waitCh:
			return nil
		case <-time.After(grace):
			killErr := killSingBoxProcess(cmd)
			select {
			case <-waitCh:
				return nil
			case <-time.After(singBoxHardKillWait):
				if killErr != nil {
					return fmt.Errorf("kill sing-box after cancellation: %w", killErr)
				}
				return errors.New("sing-box did not exit after hard kill")
			}
		}
	}
}
