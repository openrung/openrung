package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"github.com/openrung/openrung/connectcore/client"
)

// runFuncRuntime adapts a blocking run function — the shape of the old
// runTunnel seam and of client.SingBoxRunner.Run — into a TunnelRuntime, so
// ladder tests can express "runs until stopped" or "crashes with err" as one
// function. The function's context is cancelled by Stop, and a Stop-requested
// return reports nil whatever the function returned, mirroring the runner.
type runFuncRuntime func(ctx context.Context, configJSON []byte) error

func (f runFuncRuntime) Run(ctx context.Context, configJSON []byte) (TunnelRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &funcRun{cancel: cancel, done: make(chan error, 1), exited: make(chan struct{})}
	go func() {
		err := f(runCtx, configJSON)
		run.mu.Lock()
		if run.stopped {
			err = nil
		}
		run.mu.Unlock()
		run.done <- err
		close(run.done)
		close(run.exited)
	}()
	return run, nil
}

type funcRun struct {
	cancel  context.CancelFunc
	done    chan error
	exited  chan struct{}
	mu      sync.Mutex
	stopped bool
}

func (r *funcRun) Done() <-chan error { return r.done }

func (r *funcRun) Stop(time.Duration) error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	<-r.exited
	return nil
}

// tunnelRuntimeFunc adapts a bare Run function for tests that fail the launch
// itself (the path a broken temp dir or missing binary takes).
type tunnelRuntimeFunc func(ctx context.Context, configJSON []byte) (TunnelRun, error)

func (f tunnelRuntimeFunc) Run(ctx context.Context, configJSON []byte) (TunnelRun, error) {
	return f(ctx, configJSON)
}

// recordingRuntime is a fully in-memory TunnelRuntime that remembers each
// run's config bytes and Stop calls, for asserting what the engine drove
// through the seam.
type recordingRuntime struct {
	mu   sync.Mutex
	runs []*recordingRun
	// fail decides each launch's fate by 1-based attempt: a non-nil error
	// makes that run die immediately (delivered through Done, an exit nobody
	// requested); nil lets it run until stopped.
	fail func(attempt int) error
}

func (rt *recordingRuntime) Run(ctx context.Context, configJSON []byte) (TunnelRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run := &recordingRun{
		config:  append([]byte(nil), configJSON...),
		done:    make(chan error, 1),
		stopped: make(chan struct{}),
	}
	rt.mu.Lock()
	rt.runs = append(rt.runs, run)
	attempt := len(rt.runs)
	rt.mu.Unlock()
	if rt.fail != nil {
		if err := rt.fail(attempt); err != nil {
			run.done <- err
			close(run.done)
			return run, nil
		}
	}
	go func() {
		<-run.stopped
		run.done <- nil
		close(run.done)
	}()
	return run, nil
}

func (rt *recordingRuntime) run(index int) *recordingRun {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.runs[index]
}

func (rt *recordingRuntime) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.runs)
}

type recordingRun struct {
	config []byte
	done   chan error

	mu        sync.Mutex
	stopGrace []time.Duration
	stopped   chan struct{}
	stopOnce  sync.Once
}

func (r *recordingRun) Done() <-chan error { return r.done }

func (r *recordingRun) Stop(grace time.Duration) error {
	r.mu.Lock()
	r.stopGrace = append(r.stopGrace, grace)
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stopped) })
	return nil
}

func (r *recordingRun) stops() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.stopGrace...)
}

// TestStubTunnelRuntimeDrivesFullLadder is ADR-003 A1's acceptance test: a
// stub in-memory TunnelRuntime carries the engine's full connect ladder — a
// first candidate that fails its end-to-end probe and is stopped through the
// seam, promotion of the next, and the disconnect teardown — with no sing-box
// binary, no process, and no config file. It also pins the seam's terms: the
// engine hands the runtime config BYTES, and stops every run through Stop
// with the ladder's grace budget.
func TestStubTunnelRuntimeDrivesFullLadder(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{
		relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"),
		relayAt("b", "SG", "", "Singapore", "127.0.0.11"),
	}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	// Rank relay a first so the fail-then-succeed order is deterministic.
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		if host == "127.0.0.10" {
			return 5, nil
		}
		return 50, nil
	}
	// The first rung fails as a relay data-path result (the failure class that
	// advances the ladder); its run must still be stopped through the seam.
	var probes int
	var probeMu sync.Mutex
	s.probeTunnel = func(ctx context.Context, proxyPort int) (int64, error) {
		probeMu.Lock()
		defer probeMu.Unlock()
		probes++
		if probes == 1 {
			return 0, errors.New("first candidate carried no data")
		}
		return 2, nil
	}
	rt := &recordingRuntime{}
	s.TunnelRuntime = rt

	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	state := waitForStatus(t, s, StatusConnected)
	if state.RelayLabel == nil || *state.RelayLabel != "Singapore" {
		t.Fatalf("relayLabel = %v; want the second candidate after the first rung failed", state.RelayLabel)
	}
	if rt.count() != 2 {
		t.Fatalf("runtime saw %d runs; want 2 (failed rung, then winner)", rt.count())
	}
	if stops := rt.run(0).stops(); len(stops) != 1 || stops[0] != LadderKillGrace {
		t.Fatalf("failed rung Stop calls = %v; want exactly one with the ladder grace %v", stops, LadderKillGrace)
	}
	for index, wantHost := range []string{"127.0.0.10", "127.0.0.11"} {
		var generated struct {
			Outbounds []struct {
				Server string `json:"server"`
			} `json:"outbounds"`
		}
		config := rt.run(index).config
		if err := json.Unmarshal(config, &generated); err != nil {
			t.Fatalf("run %d config is not the generated JSON bytes: %v", index+1, err)
		}
		if len(generated.Outbounds) == 0 || generated.Outbounds[0].Server != wantHost {
			t.Fatalf("run %d config outbound = %+v; want server %s", index+1, generated.Outbounds, wantHost)
		}
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
	stops := rt.run(1).stops()
	if len(stops) != 1 || stops[0] != LadderKillGrace {
		t.Fatalf("winning run Stop calls = %v; want exactly one with the ladder grace %v", stops, LadderKillGrace)
	}
}

// A cancelled context aborts the launch before anything is materialized —
// the seam's stop-during-start contract.
func TestSubprocessRuntimeRefusesLaunchAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run, err := subprocessRuntime{}.Run(ctx, []byte("{}"))
	if run != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancel = %v, %v; want nil, context.Canceled", run, err)
	}
}

// A temp-file failure keeps the config_file stage it has always had, so
// telemetry keeps telling local disk trouble apart from a binary that would
// not launch.
func TestSubprocessRuntimeConfigFileFailureKeepsItsStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TMPDIR does not steer os.CreateTemp on windows")
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	run, err := subprocessRuntime{}.Run(context.Background(), []byte("{}"))
	if run != nil || err == nil {
		t.Fatalf("Run with a broken temp dir = %v, %v; want a launch error", run, err)
	}
	if stage, local := localCandidateErrorStage(err); !local || stage != "config_file" {
		t.Fatalf("stage = %q (local=%t); want the config_file local stage", stage, local)
	}
}

// The subprocess default end to end: the config bytes reach the child as the
// file the seam materialized, Stop ends a stop-protocol child through the
// stdin-close request, Done reports nil for that requested exit, and the temp
// config is gone by the time Done reports.
func TestSubprocessRuntimeMaterializesConfigAndCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub child is a shell script")
	}
	dir := t.TempDir()
	pathFile := filepath.Join(dir, "config-path")
	bodyFile := filepath.Join(dir, "config-body")
	script := filepath.Join(dir, "stubbox")
	// The body file is written last and atomically (rename), so its presence
	// means both captures are complete.
	stub := "#!/bin/sh\n" +
		"printf '%s' \"$3\" > " + pathFile + "\n" +
		"cat \"$3\" > " + bodyFile + ".tmp && mv " + bodyFile + ".tmp " + bodyFile + "\n" +
		"cat > /dev/null\n" + // stop protocol: block until stdin EOF
		"exit 0\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	configJSON := []byte(`{"stub":"config bytes, never a path requirement"}`)
	rt := subprocessRuntime{runner: client.SingBoxRunner{Path: script, StopOnStdinClose: true}}
	run, err := rt.Run(context.Background(), configJSON)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Wait for the child to reach its stdin read (process spawn can be slow)
	// before stopping it; an early Done is a child that died on its own.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(bodyFile); err == nil {
			break
		}
		select {
		case err := <-run.Done():
			t.Fatalf("run exited before it was stopped: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("child never captured the materialized config")
		}
	}
	if err := run.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-run.Done():
		if err != nil {
			t.Fatalf("Done after a requested stop = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Done never reported after Stop")
	}

	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatalf("child never saw the materialized config: %v", err)
	}
	if string(body) != string(configJSON) {
		t.Fatalf("child read %q; want the config bytes %q", body, configJSON)
	}
	tempPath, err := os.ReadFile(pathFile)
	if err != nil {
		t.Fatalf("child recorded no config path: %v", err)
	}
	if !strings.Contains(filepath.Base(string(tempPath)), "openrung-proxy-") {
		t.Fatalf("temp config path = %q; want the openrung-proxy-* temp name", tempPath)
	}
	if _, err := os.Stat(string(tempPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp config survived the run's exit: %v", err)
	}
}
