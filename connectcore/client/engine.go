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
	// KillGrace bounds the wait between the stop request Run sends on context
	// cancel (stdin close and/or interrupt) and the hard kill. Zero keeps the
	// 5s default. Only Run consults it; a Start caller passes its grace to
	// SingBoxProcess.Stop directly (the connectcore engine shortens it for
	// failed ladder candidates: an external binary on Windows receives no stop
	// request at all — os.Interrupt is unsupported there — so without a short
	// grace every failed candidate's teardown would cost the full default).
	KillGrace time.Duration
	// StopOnStdinClose declares that the binary at Path speaks the stdin-close
	// stop protocol (see StopOnStdinCloseFlag). The bundled runtime does; an
	// external sing-box does not and would refuse the unknown flag, so this
	// must stay false for one. When set, Run passes the flag, keeps a stdin
	// pipe open for the child's lifetime, and closes it on cancellation before
	// the interrupt-then-kill ladder runs.
	StopOnStdinClose bool
}

// Run supervises one sing-box run to completion: it returns the decorated
// exit error when the child dies on its own, and on context cancellation it
// stops the child (gracefully, then forced after KillGrace) and returns the
// stop's outcome. It is Start + Done/Stop composed for hosts that want the
// whole lifetime as one blocking call (cmd/wssmatrix does).
func (r SingBoxRunner) Run(ctx context.Context, configPath string) error {
	process, err := r.Start(configPath)
	if err != nil {
		return err
	}
	select {
	case err := <-process.Done():
		return err
	case <-ctx.Done():
		return process.Stop(r.KillGrace)
	}
}

// Start launches sing-box and returns a handle for the started process. It
// returns at *launched*, not *ready* — readiness stays the caller's job (the
// connectcore engine proves it with its own startup verification). This is
// the subprocess half of the engine's TunnelRuntime seam (ADR-003 A1): every
// engine start/stop runs through Start, SingBoxProcess.Done, and
// SingBoxProcess.Stop.
func (r SingBoxRunner) Start(configPath string) (*SingBoxProcess, error) {
	if configPath == "" {
		return nil, errors.New("sing-box config path is required")
	}

	binary := r.Path
	if binary == "" {
		binary = "sing-box"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find sing-box %q: %w", binary, err)
	}

	args := []string{"run", "-c", configPath}
	if r.StopOnStdinClose {
		args = append(args, "-"+StopOnStdinCloseFlag)
	}
	cmd := exec.Command(resolved, args...)
	configureSingBoxProcess(cmd)

	// The stop pipe (see StopOnStdinCloseFlag). Both ends stay open for the
	// child's lifetime: the write end held open is what makes the child's
	// stdin read block, and closing it is the stop request. The supervision
	// goroutine closes both once the child exits; on the hard-kill-timeout
	// path — Stop returning while the child may still be alive — the write
	// end was already closed by the stop request, so the EOF still reaches it.
	var stopPipeRead, stopPipeWrite *os.File
	if r.StopOnStdinClose {
		stdinRead, stdinWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, fmt.Errorf("create sing-box stop pipe: %w", pipeErr)
		}
		cmd.Stdin = stdinRead
		stopPipeRead, stopPipeWrite = stdinRead, stdinWrite
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
		if stopPipeRead != nil {
			_ = stopPipeRead.Close()
			_ = stopPipeWrite.Close()
		}
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	process := &SingBoxProcess{
		cmd:           cmd,
		stopPipeRead:  stopPipeRead,
		stopPipeWrite: stopPipeWrite,
		stdout:        stdout,
		stderr:        stderr,
		release:       func() {},
		exited:        make(chan struct{}),
		done:          make(chan error, 1),
	}
	// Windows: a kill-on-close job object so an external binary — which speaks
	// no stop protocol — cannot outlive a runner that died without teardown
	// (the Unix builds get this from the process group). It must NEVER hold a
	// stop-protocol child: on this process's death the kernel closes the job
	// handle and the stop pipe in no defined order, and closing the job's last
	// handle terminates the child IMMEDIATELY — it would lose the race to
	// observe EOF and unwind its routes and DNS, defeating the pipe's graceful
	// orphan teardown, which is that child's parent-death cleanup.
	// Best-effort; see the Windows implementation.
	if !r.StopOnStdinClose {
		process.release = superviseSingBoxProcess(cmd)
	}
	go process.supervise()
	return process, nil
}

// SingBoxProcess is one started sing-box run. Its exit is reported exactly
// once through Done; Stop requests a graceful stop and blocks until the child
// has exited (or the forced-kill budget expires). A fresh handle per run —
// there is no restart — so a Done report is never ambiguous about which run
// it belongs to.
type SingBoxProcess struct {
	cmd           *exec.Cmd
	stopPipeRead  *os.File
	stopPipeWrite *os.File
	stdout        *crashLineRecorder
	stderr        *crashLineRecorder
	release       func()

	mu            sync.Mutex
	stopRequested bool

	stopSignal  sync.Once
	releaseOnce sync.Once
	exited      chan struct{} // closed when cmd.Wait has returned
	done        chan error    // the decorated exit report; sent once, then closed
}

// supervise owns the process's exit: it reaps the child, releases the
// platform supervision, frees the stop pipe, and delivers the run's exit
// report to Done — the decorated crash quote for an exit nobody requested,
// nil for one that Stop requested.
func (p *SingBoxProcess) supervise() {
	waitErr := p.cmd.Wait()
	p.mu.Lock()
	stopRequested := p.stopRequested
	p.mu.Unlock()
	close(p.exited)
	p.releaseOnce.Do(p.release)
	if p.stopPipeRead != nil {
		_ = p.stopPipeRead.Close()
		// Concurrent-close-safe against the stop request's own close.
		_ = p.stopPipeWrite.Close()
	}
	switch {
	case stopRequested:
		p.done <- nil
	case waitErr != nil:
		if line := firstCrashLine(p.stderr, p.stdout); line != "" {
			p.done <- &crashError{quote: line, cause: waitErr}
		} else {
			p.done <- fmt.Errorf("sing-box exited: %w", waitErr)
		}
	default:
		p.done <- errors.New("sing-box exited")
	}
	close(p.done)
}

// Done delivers the run's exit report exactly once, then the channel is
// closed: non-nil for an exit nobody requested (with the child's own crash
// line when it said one — see crashError), nil for an exit Stop requested.
func (p *SingBoxProcess) Done() <-chan error { return p.done }

// Stop requests a graceful stop and blocks until the child exits, escalating
// to a hard kill after grace (zero or negative keeps the 5s default) and
// giving up singBoxHardKillWait after that. Both stop channels fire: the pipe
// close is the one a bundled child acts on everywhere including Windows, the
// interrupt is what an external binary understands (and is harmlessly refused
// on Windows). Safe on a child that already exited; the returned error
// reports only a stop that failed to take effect.
func (p *SingBoxProcess) Stop(grace time.Duration) error {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	p.mu.Lock()
	p.stopRequested = true
	p.mu.Unlock()
	p.stopSignal.Do(func() {
		if p.stopPipeWrite != nil {
			_ = p.stopPipeWrite.Close()
		}
		_ = interruptSingBoxProcess(p.cmd)
	})
	select {
	case <-p.exited:
		return nil
	case <-time.After(grace):
	}
	killErr := killSingBoxProcess(p.cmd)
	select {
	case <-p.exited:
		return nil
	case <-time.After(singBoxHardKillWait):
		// One more attempt to take the child down before giving up: on
		// Windows, closing the job handle terminates a stopless child
		// (kill-on-close) — the backstop Run's deferred release used to
		// provide here.
		p.releaseOnce.Do(p.release)
		if killErr != nil {
			return fmt.Errorf("kill sing-box after cancellation: %w", killErr)
		}
		return errors.New("sing-box did not exit after hard kill")
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
