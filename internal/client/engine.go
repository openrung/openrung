package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

type SingBoxRunner struct {
	Path   string
	Stdout io.Writer
	Stderr io.Writer
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
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-waitCh:
			return nil
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
			return nil
		}
	}
}
