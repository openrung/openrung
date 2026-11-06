// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/openrung/openrung/connectcore/client"
)

func startFakeIPCandidate(t *testing.T, s *Engine, input client.SingBoxConfigInput) (*candidateResult, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return s.startCandidate(&candidateResult{ctx: ctx, cancel: cancel}, input)
}

func fakeIPInput(s *Engine) client.SingBoxConfigInput {
	return s.candidateConfigInput(relayAt("a", "JP", "Tokyo", "Japan", "127.0.0.10"), 0)
}

func assertFakeIPLocked(t *testing.T, cache string) {
	t.Helper()
	lock, err := acquireFakeIPLock(cache)
	if lock != nil {
		_ = lock.Close()
	}
	if !errors.Is(err, errFakeIPCacheInUse) {
		t.Fatalf("expected exclusive ownership, got %v", err)
	}
}

func TestEngineFakeIPRejectsSecondOwnerBeforeStateAccess(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	firstRT := &cleanRuntime{clean: true}
	first := fakeIPTestEngine(t, cache, firstRT)
	owner, err := startFakeIPCandidate(t, first, fakeIPInput(first))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.teardown)
	if err := os.WriteFile(cache, []byte("retained mappings"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(fakeIPRunStatePath(cache))
	if err != nil {
		t.Fatal(err)
	}
	// Both fake-IP and bypass candidates must stop before launch, without a
	// real-DNS retry and without rewriting either of the owner's files.
	for _, bypass := range []bool{false, true} {
		secondRT := &cleanRuntime{clean: true}
		second := fakeIPTestEngine(t, cache, secondRT)
		input := fakeIPInput(second)
		input.SplitTunnel = &client.SplitTunnelRules{BypassLAN: bypass}
		_, err := startFakeIPCandidate(t, second, input)
		if !errors.Is(err, errFakeIPCacheInUse) || len(secondRT.configs) != 0 {
			t.Fatalf("second owner: err=%v, launches=%d", err, len(secondRT.configs))
		}
		if _, local := localCandidateErrorStage(err); !local {
			t.Fatal("ownership conflict must stop the relay ladder")
		}
		if got, err := os.ReadFile(cache); err != nil || string(got) != "retained mappings" {
			t.Fatalf("second owner changed cache: %q, %v", got, err)
		}
		if got, err := os.ReadFile(fakeIPRunStatePath(cache)); err != nil || !bytes.Equal(got, state) {
			t.Fatalf("second owner changed run state: %q, %v", got, err)
		}
	}
	owner.teardown()
	startFakeIPTestRun(t, first)
	if fakeIPv4Range(t, firstRT.configs[0]) != fakeIPv4Range(t, firstRT.configs[1]) {
		t.Fatal("handoff did not retain the clean owner's block")
	}
}

func TestEngineFakeIPOwnershipCoversFallbackAndLaunchFailure(t *testing.T) {
	for _, mode := range []string{"fallback", "state failure", "bypass", "launch failure", "config failure", "lock failure"} {
		t.Run(mode, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "fakeip.db")
			rt := &cleanRuntime{clean: true}
			s := fakeIPTestEngine(t, cache, rt)
			input := fakeIPInput(s)
			switch mode {
			case "fallback":
				rt.fail = func([]byte) error {
					assertFakeIPLocked(t, cache)
					if len(rt.configs) == 1 {
						return errors.New("cache startup failed")
					}
					return nil
				}
			case "bypass":
				input.SplitTunnel = &client.SplitTunnelRules{BypassLAN: true}
			case "state failure":
				if err := os.Mkdir(fakeIPRunStatePath(cache), 0o700); err != nil {
					t.Fatal(err)
				}
			case "launch failure":
				rt.fail = func([]byte) error { return errors.New("runtime unavailable") }
			case "config failure":
				input.MTU = -1
			case "lock failure":
				input.FakeIPCachePath = filepath.Join(cache, "missing", "fakeip.db")
			}
			res, err := startFakeIPCandidate(t, s, input)
			if mode == "fallback" || mode == "state failure" || mode == "bypass" {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(res.teardown)
				assertFakeIPLocked(t, cache)
				if fakeIPv4Range(t, rt.configs[len(rt.configs)-1]) != "" {
					t.Fatal("expected real DNS")
				}
				res.teardown()
			} else if err == nil {
				res.teardown()
				t.Fatal("expected startup failure")
			}
			if mode == "lock failure" && len(rt.configs) != 0 {
				t.Fatal("lock failure launched a runtime")
			}
			lock, err := acquireFakeIPLock(cache)
			if err != nil {
				t.Fatalf("ownership leaked after teardown: %v", err)
			}
			_ = lock.Close()
		})
	}
}

type failedStopFakeIPRun struct{ done chan error }

func (r *failedStopFakeIPRun) Done() <-chan error { return r.done }
func (r *failedStopFakeIPRun) Stop(time.Duration) error {
	return errors.New("runtime still alive")
}

func TestEngineFakeIPRetainsOwnershipUntilFailedStopActuallyExits(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	run := &failedStopFakeIPRun{done: make(chan error)}
	rt := tunnelRuntimeFunc(func(context.Context, []byte) (TunnelRun, error) { return run, nil })
	s := fakeIPTestEngine(t, cache, rt)
	res, err := startFakeIPCandidate(t, s, fakeIPInput(s))
	if err != nil {
		t.Fatal(err)
	}
	res.teardown()
	assertFakeIPLocked(t, cache)
	close(run.done)
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := acquireFakeIPLock(cache)
		if err == nil {
			_ = lock.Close()
			break
		}
		if !errors.Is(err, errFakeIPCacheInUse) || time.Now().After(deadline) {
			t.Fatalf("ownership not released after exit: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if state, err := readFakeIPRunState(cache); err != nil || state.Clean {
		t.Fatalf("unclean exit must keep dirty state: %+v, %v", state, err)
	}
}

func TestEngineFakeIPDoesNotRetryWhileFailedStartupStillRuns(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	run := &failedStopFakeIPRun{done: make(chan error)}
	defer close(run.done)
	launches := 0
	rt := tunnelRuntimeFunc(func(context.Context, []byte) (TunnelRun, error) {
		launches++
		return run, nil
	})
	s := fakeIPTestEngine(t, cache, rt)
	s.tunnelReady = func(context.Context, int) error { return errors.New("inbound not ready") }
	s.tunnelReadyLimit = 30 * time.Millisecond
	res, err := startFakeIPCandidate(t, s, fakeIPInput(s))
	if err == nil || res != nil || launches != 1 {
		t.Fatalf("startup: result=%v, err=%v, launches=%d", res, err, launches)
	}
	assertFakeIPLocked(t, cache)
}

func TestFakeIPLockAcrossProcesses(t *testing.T) {
	if cache := os.Getenv("OPENRUNG_FAKEIP_LOCK_HELPER"); cache != "" {
		lock, err := acquireFakeIPLock(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if _, err := beginFakeIPRun(cache); err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	cache := filepath.Join(t.TempDir(), "fakeip.db")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFakeIPLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "OPENRUNG_FAKEIP_LOCK_HELPER="+cache)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire ownership: %q, %v", line, err)
	}
	assertFakeIPLocked(t, cache)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := acquireFakeIPLock(cache)
	if err != nil {
		t.Fatalf("process death did not release ownership: %v", err)
	}
	defer lock.Close()
	before, err := readFakeIPRunState(cache)
	if err != nil {
		t.Fatal(err)
	}
	block, err := beginFakeIPRun(cache)
	if err != nil || block != (before.Block+1)%client.FakeIPBlockCount {
		t.Fatalf("crash handoff: block=%d, previous=%+v, err=%v", block, before, err)
	}
}
