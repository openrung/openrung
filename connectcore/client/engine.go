package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const singBoxHardKillWait = 2 * time.Second

// StopOnStdinCloseFlag names the run-subcommand flag (minus the leading dash)
// of the graceful-stop protocol between this runner and the bundled sing-box
// runtime: a child started with the flag treats its stdin reaching EOF as a
// stop request and unwinds exactly like an interrupt — the TUN device, routes,
// and DNS settings come back down before it exits. The runner passes the flag
// and holds the pipe's write end for the child's lifetime whenever
// StopOnStdinClose is set.
//
// The pipe is a stop channel that needs no signal delivery, which makes it the
// one that works on Windows (os.Interrupt cannot be sent there, and no console
// control event reaches a CREATE_NO_WINDOW child). It also closes a gap
// signals never covered on any platform: the OS closes the write end when this
// process dies for ANY reason, so a tunnel child orphaned by a crashed or
// force-killed host still unwinds its routes and DNS instead of leaving the
// device captured by a tunnel nobody supervises.
const StopOnStdinCloseFlag = "stop-on-stdin-close"

type SingBoxRunner struct {
	Path   string
	Stdout io.Writer
	Stderr io.Writer
	// KillGrace bounds the wait between the stop request sent on context
	// cancel (stdin close and/or interrupt) and the hard kill. Zero keeps the
	// 5s default. The desktop connect ladder shortens it: an external binary
	// on Windows receives no stop request at all (os.Interrupt is unsupported
	// there), so without a short grace every failed candidate's teardown would
	// cost the full default.
	KillGrace time.Duration
	// StopOnStdinClose declares that the binary at Path speaks the stdin-close
	// stop protocol (see StopOnStdinCloseFlag). The bundled runtime does; an
	// external sing-box does not and would refuse the unknown flag, so this
	// must stay false for one. When set, Run passes the flag, keeps a stdin
	// pipe open for the child's lifetime, and closes it on cancellation before
	// the interrupt-then-kill ladder runs.
	StopOnStdinClose bool
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

	args := []string{"run", "-c", configPath}
	if r.StopOnStdinClose {
		args = append(args, "-"+StopOnStdinCloseFlag)
	}
	cmd := exec.Command(resolved, args...)
	configureSingBoxProcess(cmd)

	// The stop pipe (see StopOnStdinCloseFlag). Both ends stay open until Run
	// returns: the write end held open is what makes the child's stdin read
	// block, and closing it is the stop request. The deferred closes also
	// cover the hard-kill-timeout path, where Run returns while the child may
	// still be alive — the EOF then still reaches it.
	var stopPipe *os.File
	if r.StopOnStdinClose {
		stdinRead, stdinWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			return fmt.Errorf("create sing-box stop pipe: %w", pipeErr)
		}
		defer stdinRead.Close()
		defer stdinWrite.Close()
		cmd.Stdin = stdinRead
		stopPipe = stdinWrite
	}
	// Each stream gets its own recorder — a shared one would splice a partial
	// stderr line together with stdout chatter into a misleading quote — and
	// the recorders remember sing-box's own words so an abnormal exit reports
	// more than a bare exit status: the user-facing Error row is built from
	// the returned error, and "exit status 1" alone sends users to the logs
	// for something like a missing with_utls build tag the child said out
	// loud.
	//
	// A caller that passed the SAME writer for both streams was relying on
	// os/exec's serialized-Write guarantee for cmd.Stdout == cmd.Stderr; two
	// distinct recorders void that on cmd, so the shared downstream writer
	// gets an explicit lock instead.
	stdoutNext, stderrNext := r.Stdout, r.Stderr
	if stdoutNext != nil && sameWriter(r.Stdout, r.Stderr) {
		shared := &syncWriter{w: stdoutNext}
		stdoutNext, stderrNext = shared, shared
	}
	stdout := &crashLineRecorder{next: stdoutNext}
	stderr := &crashLineRecorder{next: stderrNext}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}
	// Windows: a kill-on-close job object so no child outlives a runner that
	// died without teardown (the Unix builds get this from the process group +
	// the stop pipe). Best-effort; see the Windows implementation.
	releaseSupervision := superviseSingBoxProcess(cmd)
	defer releaseSupervision()

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
		// Both stop channels fire: the pipe close is the one a bundled child
		// acts on everywhere including Windows, the interrupt is what an
		// external binary understands (and is harmlessly refused on Windows).
		if stopPipe != nil {
			_ = stopPipe.Close()
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

// syncWriter serializes Write calls to a downstream writer both recorders
// share: exec copies the two streams on separate goroutines, and the caller's
// single writer must still see one Write at a time — the guarantee os/exec
// itself gives when cmd.Stdout == cmd.Stderr.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
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
// report failures, so it is passed first.
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
