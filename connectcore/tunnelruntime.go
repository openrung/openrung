package connectcore

import (
	"context"
	"os"
	"time"

	"github.com/openrung/openrung/connectcore/client"
)

// TunnelRuntime executes the tunnel core the engine drives (ADR-003 Track A,
// A1). Desktop and the TUI run sing-box as a child process — the default
// implementation below, built on client.SingBoxRunner — while the mobile
// binding runs libbox in the same process and injects its own implementation
// through Engine.TunnelRuntime. The engine itself never constructs or signals
// a process except through this seam.
//
// The contract every implementation carries:
//
//   - Run receives the generated sing-box config as bytes. An implementation
//     may materialize a file (the subprocess one does), but the seam never
//     requires a path.
//   - Run returns at *launched*, not *ready*. Readiness stays the engine's
//     job, proven by its own startup verification (awaitTunnelReady), exactly
//     as it is for both current worlds — the desktop subprocess and the
//     mobile-native state machines.
//   - Context cancellation aborts a launch. Once Run has returned, the run's
//     lifetime belongs to the returned handle: stopping a live run is
//     TunnelRun.Stop's job, graceful then forced within its grace budget.
//   - A launch failure returned by Run, and an exit delivered through Done,
//     must carry enough for the shared failure classifier
//     (clienttelemetry.ClassifyError). The subprocess implementation quotes
//     the child's own crash line while keeping a telemetry-safe rendering
//     (see client.SingBoxRunner); an in-process implementation should
//     surface the core's error text the same way.
type TunnelRuntime interface {
	Run(ctx context.Context, configJSON []byte) (TunnelRun, error)
}

// TunnelRun is one live tunnel run. Every attempt gets a fresh handle, as
// every attempt is a fresh process on desktop (ADR-003 open decision 1,
// shape 2): per-run handles leave no ambiguity about which run a Done report
// belongs to across the engine's restart paths (WSS fallback, failover
// re-ladder), and make an in-process implementation's between-runs teardown
// obligation explicit.
type TunnelRun interface {
	// Done delivers the run's exit report exactly once, then the channel is
	// closed: non-nil for an exit nobody requested — the error feeds the
	// shared failure classifier — and nil for an exit Stop requested.
	Done() <-chan error
	// Stop requests a graceful stop and blocks until the run has ended,
	// escalating to a forced stop after grace (an implementation may keep a
	// small fixed budget for the forced phase). Zero or negative grace means
	// the implementation's default. Safe on a run that already exited; the
	// returned error reports only a stop that failed to take effect.
	Stop(grace time.Duration) error
}

// tunnelRuntime resolves the runtime that executes this candidate's tunnel
// core: the host-injected one, else the subprocess default.
func (s *Engine) tunnelRuntime() TunnelRuntime {
	if s.TunnelRuntime != nil {
		return s.TunnelRuntime
	}
	return subprocessRuntime{runner: client.SingBoxRunner{
		Path:             s.SingBoxPath,
		Stdout:           s.logWriter(),
		Stderr:           s.logWriter(),
		StopOnStdinClose: s.SingBoxStopsOnStdinClose,
	}}
}

// subprocessRuntime is the desktop/TUI default: each run writes the config
// bytes to a temp file, starts sing-box through client.SingBoxRunner, and
// removes the file once that run's process has exited.
type subprocessRuntime struct {
	runner client.SingBoxRunner
}

// Run materializes the config, launches sing-box, and rechecks the launch
// context at every stage: cancellation observed before, during, or right
// after the launch unwinds whatever exists — file removed, a started child
// stopped — and returns the context error, never a live run (the seam's
// stop-during-start contract).
func (rt subprocessRuntime) Run(ctx context.Context, configJSON []byte) (TunnelRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	configPath, err := writeTempConfig(configJSON)
	if err != nil {
		// The engine folds Run errors into the tunnel_start stage; a temp-file
		// failure keeps the stage it has always had, so telemetry can tell
		// local disk trouble from a binary that would not launch.
		return nil, markLocalCandidateError("config_file", err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(configPath)
		return nil, err
	}
	process, err := rt.runner.Start(configPath)
	if err != nil {
		_ = os.Remove(configPath)
		return nil, err
	}
	run := &subprocessRun{
		process:    process,
		configPath: configPath,
		done:       make(chan error, 1),
		cleaned:    make(chan struct{}),
	}
	go run.forward()
	if err := ctx.Err(); err != nil {
		// The launch was cancelled while the child came up: unwind it before
		// answering, so a disconnect racing the launch never yields a live run.
		_ = run.Stop(0)
		return nil, err
	}
	return run, nil
}

// subprocessRun forwards the process's exit report so the config file's
// removal is ordered before Done observes the exit, and marks cleaned so
// Stop can block until the file is off disk.
type subprocessRun struct {
	process    *client.SingBoxProcess
	configPath string
	done       chan error
	cleaned    chan struct{}
}

func (r *subprocessRun) forward() {
	exitErr := <-r.process.Done()
	// Only after the exit: sing-box may re-read its config while alive.
	_ = os.Remove(r.configPath)
	close(r.cleaned)
	r.done <- exitErr
	close(r.done)
}

func (r *subprocessRun) Done() <-chan error { return r.done }

// Stop stops the child and, once it has terminated, also waits for the run's
// cleanup — the temp config (which carries the relay's credentials) must be
// off disk before Stop returns, so a host that exits right after a shutdown
// Stop never leaves it behind. On a stop that failed to take effect the
// child may still be alive; cleanup then happens whenever it finally exits.
func (r *subprocessRun) Stop(grace time.Duration) error {
	if err := r.process.Stop(grace); err != nil {
		return err
	}
	<-r.cleaned
	return nil
}
