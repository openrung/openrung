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
	"unicode/utf8"
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
	//
	// Except when the caller passed the SAME writer for both streams: os/exec
	// serializes Write calls only while cmd.Stdout == cmd.Stderr, so wrapping
	// one writer in two recorders would silently void that contract and race
	// the caller's writer. One shared recorder preserves it.
	stdout := &crashLineRecorder{next: r.Stdout}
	stderr := stdout
	if !sameWriter(r.Stdout, r.Stderr) {
		stderr = &crashLineRecorder{next: r.Stderr}
	}
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
				return &crashError{quote: line, cause: err}
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

// crashError separates the child's quoted output from the telemetry-safe
// cause: Error carries the quote for user-facing surfaces (the TUI Error row,
// the desktop banner), while TelemetrySafe returns only the process-exit fact
// — raw child output can name local paths and usernames, which must never
// reach the broker (clienttelemetry.ErrorDetail honors this).
type crashError struct {
	quote string
	cause error
}

func (e *crashError) Error() string {
	return fmt.Sprintf("sing-box exited: %s (%v)", e.quote, e.cause)
}

func (e *crashError) Unwrap() error { return e.cause }

func (e *crashError) TelemetrySafe() string {
	return "sing-box exited: " + e.cause.Error()
}

// sameWriter mirrors os/exec's interfaceEqual: comparing writers with
// uncomparable dynamic types panics, which must read as "different", not
// crash the runner.
func sameWriter(a, b io.Writer) (same bool) {
	defer func() { _ = recover() }()
	return a == b
}

const (
	// crashLineRecorderMaxLine bounds the memory a single unbroken output
	// line can pin; longer lines are flushed at a rune boundary as if a
	// newline had arrived.
	crashLineRecorderMaxLine = 512
	// crashLineTailLen bounds how far back a crash quote may reach: an
	// error-looking line from long before the exit is more likely a recovered
	// transient than the cause, so only the final lines are candidates.
	crashLineTailLen = 8
)

type crashLine struct {
	text  string
	isErr bool
}

// crashLineRecorder tees writes through to next while remembering the child's
// final few lines, so an abnormal exit can quote the process instead of just
// its exit status.
type crashLineRecorder struct {
	next io.Writer

	mu      sync.Mutex
	partial []byte
	tail    []crashLine // last crashLineTailLen non-empty lines, oldest first
}

func (r *crashLineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	for _, b := range p {
		if b == '\n' {
			r.recordLocked()
			r.partial = r.partial[:0]
			continue
		}
		if len(r.partial) >= crashLineRecorderMaxLine {
			r.flushLongLineLocked()
		}
		r.partial = append(r.partial, b)
	}
	r.mu.Unlock()

	if r.next == nil {
		return len(p), nil
	}
	return r.next.Write(p)
}

// flushLongLineLocked records the over-long buffered line, holding back any
// trailing incomplete UTF-8 rune so the forced cut cannot split one — a
// dangling partial rune would put mojibake in the user-facing quote.
func (r *crashLineRecorder) flushLongLineLocked() {
	cut := len(r.partial)
	for i := 0; i < utf8.UTFMax && cut > 0; i++ {
		if c, size := utf8.DecodeLastRune(r.partial[:cut]); c != utf8.RuneError || size > 1 {
			break
		}
		cut--
	}
	if cut == 0 {
		cut = len(r.partial) // not UTF-8 at all; cut arbitrarily rather than stall
	}
	rest := append([]byte(nil), r.partial[cut:]...)
	r.partial = r.partial[:cut]
	r.recordLocked()
	r.partial = append(r.partial[:0], rest...)
}

func (r *crashLineRecorder) recordLocked() {
	line := strings.TrimSpace(string(r.partial))
	if line == "" {
		return
	}
	lower := strings.ToLower(line)
	isErr := strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic")
	// The bundled run shim prefixes its report with "error: "; drop it, since
	// the quote lands inside an error chain that already says so.
	line = strings.TrimSpace(strings.TrimPrefix(line, "error:"))
	if len(r.tail) == crashLineTailLen {
		copy(r.tail, r.tail[1:])
		r.tail = r.tail[:crashLineTailLen-1]
	}
	r.tail = append(r.tail, crashLine{text: line, isErr: isErr})
}

// tailLines flushes any unterminated final line (a crashing process rarely
// ends its last message with a newline) and returns the recorded tail.
func (r *crashLineRecorder) tailLines() []crashLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordLocked()
	r.partial = r.partial[:0]
	return append([]crashLine(nil), r.tail...)
}

// firstCrashLine quotes an error-looking line from ANY stream's final lines
// before settling for a stream's last chatter: benign stderr noise must not
// outrank an actual failure report on stdout. Within each tier the earlier
// recorders win — stderr is where both sing-box and the bundled run shim
// report failures, so it is passed first. Passing the same recorder twice
// (shared-writer mode) is harmless.
func firstCrashLine(recorders ...*crashLineRecorder) string {
	tails := make([][]crashLine, len(recorders))
	for i, rec := range recorders {
		tails[i] = rec.tailLines()
	}
	for _, tail := range tails {
		for i := len(tail) - 1; i >= 0; i-- {
			if tail[i].isErr {
				return tail[i].text
			}
		}
	}
	for _, tail := range tails {
		if len(tail) > 0 {
			return tail[len(tail)-1].text
		}
	}
	return ""
}
