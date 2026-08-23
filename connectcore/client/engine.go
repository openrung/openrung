package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
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
	// Each stream gets its own recorder (interleaved chunks from shared
	// recording would splice half-lines together), remembering sing-box's own
	// words so an abnormal exit reports more than a bare exit status — the
	// user-facing Error row is built from the returned error, and "exit
	// status 1" alone sends users to the logs for something like a missing
	// with_utls build tag that the child said out loud.
	stdout := &crashLineRecorder{next: r.Stdout}
	stderr := &crashLineRecorder{next: r.Stderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

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
			if line := firstCrashLine(stderr, stdout); line != "" {
				return fmt.Errorf("sing-box exited: %s (%w)", line, err)
			}
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

// crashLineRecorderMaxLine bounds the memory a single unbroken output line can
// pin; anything longer is flushed as if a newline had arrived.
const crashLineRecorderMaxLine = 512

// crashLineRecorder tees writes through to next while remembering the child's
// most recent error-looking line (and, failing that, its last non-empty line)
// so an abnormal exit can quote the process instead of just its exit status.
type crashLineRecorder struct {
	next io.Writer

	mu      sync.Mutex
	partial []byte
	errLine string
	last    string
}

func (r *crashLineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	for _, b := range p {
		if b == '\n' || len(r.partial) >= crashLineRecorderMaxLine {
			r.recordLocked()
			r.partial = r.partial[:0]
		}
		if b != '\n' {
			r.partial = append(r.partial, b)
		}
	}
	r.mu.Unlock()

	if r.next == nil {
		return len(p), nil
	}
	return r.next.Write(p)
}

func (r *crashLineRecorder) recordLocked() {
	line := strings.TrimSpace(string(r.partial))
	if line == "" {
		return
	}
	lower := strings.ToLower(line)
	// The bundled run shim prefixes its report with "error: "; drop it, since
	// the quote lands inside an error chain that already says so.
	line = strings.TrimSpace(strings.TrimPrefix(line, "error:"))
	r.last = line
	if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic") {
		r.errLine = line
	}
}

// lines flushes any unterminated final line (a crashing process rarely ends
// its last message with a newline) and returns the recorded error-looking
// line and last non-empty line.
func (r *crashLineRecorder) lines() (errLine, last string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordLocked()
	r.partial = r.partial[:0]
	return r.errLine, r.last
}

// firstCrashLine quotes an error-looking line from ANY stream before settling
// for a stream's last chatter: benign stderr noise must not outrank an actual
// failure report on stdout. Within each tier the earlier recorders win —
// stderr is where both sing-box and the bundled run shim report failures, so
// it is passed first.
func firstCrashLine(recorders ...*crashLineRecorder) string {
	lasts := make([]string, 0, len(recorders))
	for _, rec := range recorders {
		errLine, last := rec.lines()
		if errLine != "" {
			return errLine
		}
		lasts = append(lasts, last)
	}
	for _, last := range lasts {
		if last != "" {
			return last
		}
	}
	return ""
}
