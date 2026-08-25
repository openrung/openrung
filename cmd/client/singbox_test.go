package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/connectcore/client"
)

// TestRunSubcommandExitsNonzeroOnStartFailure proves the supervision contract
// end to end at the process level: connectcore's SingBoxRunner detects tunnel
// death by child exit status, so a config that parses but fails Start (here:
// its inbound port is already bound) must terminate `<binary> run -c <config>`
// with a nonzero exit code and the start error on stderr — never a hang or a
// clean exit. The test re-execs its own binary and hands control to main(),
// exercising the real error→exit-1 mapping rather than a reimplementation.
func TestRunSubcommandExitsNonzeroOnStartFailure(t *testing.T) {
	if os.Getenv("OPENRUNG_TEST_RUN_CHILD") == "1" {
		os.Args = []string{"openrung-client", "run", "-c", os.Getenv("OPENRUNG_TEST_RUN_CONFIG")}
		main()
		// main exits itself; reaching here means the run subcommand returned
		// no error for a config that cannot start.
		fmt.Fprintln(os.Stderr, "main returned instead of exiting nonzero")
		os.Exit(42)
	}

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	config := fmt.Sprintf(`{
  "log": {"level": "warn"},
  "inbounds": [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": %d}],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`, port)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunSubcommandExitsNonzeroOnStartFailure$")
	cmd.Env = append(os.Environ(),
		"OPENRUNG_TEST_RUN_CHILD=1",
		"OPENRUNG_TEST_RUN_CONFIG="+configPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited cleanly (err=%v); SingBoxRunner would report a healthy tunnel that never started. Output:\n%s", err, output)
	}
	if code := exitErr.ExitCode(); code <= 0 {
		t.Fatalf("child exit code = %d, want a positive failure code. Output:\n%s", code, output)
	}
	// The stderr content pins the failure to the runtime's Start error path,
	// not a test-harness failure that would share exit code 1.
	if !strings.Contains(string(output), "start sing-box service") {
		t.Fatalf("child stderr does not carry the start error; got:\n%s", output)
	}
}

// childOutput is a concurrency-safe sink for the stop-channel child's output:
// the test reads it in failure paths while exec's copier may still be writing.
type childOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *childOutput) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *childOutput) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// TestRunSubcommandStopsOnStdinClose proves the stdin-close stop channel end
// to end with the REAL bundled runtime: a `run -c <config>
// -stop-on-stdin-close` child brings its inbound up, and closing its stdin
// makes it close the sing-box instance and exit 0 — the graceful path every
// Windows teardown depends on, where the engine can deliver no signal at all.
// Like TestRunSubcommandExitsNonzeroOnStartFailure, the child is this test
// binary re-exec'd through main(), so the real argv dispatch and error→exit
// mapping are on the hook too.
func TestRunSubcommandStopsOnStdinClose(t *testing.T) {
	if os.Getenv("OPENRUNG_TEST_STDIN_STOP_CHILD") == "1" {
		os.Args = []string{
			"openrung-client", "run",
			"-c", os.Getenv("OPENRUNG_TEST_RUN_CONFIG"),
			"-" + client.StopOnStdinCloseFlag,
		}
		main()
		// main() returning (rather than os.Exit(1)) means the run subcommand
		// reported no error: the graceful stop. Make it exit code 0 explicitly
		// instead of letting the test harness print its own summary.
		os.Exit(0)
	}

	// A freshly released loopback port for the mixed inbound. The tiny window
	// for another process to grab it is acceptable in a test.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	config := fmt.Sprintf(`{
  "log": {"level": "warn"},
  "inbounds": [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": %d}],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`, port)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunSubcommandStopsOnStdinClose$")
	cmd.Env = append(os.Environ(),
		"OPENRUNG_TEST_STDIN_STOP_CHILD=1",
		"OPENRUNG_TEST_RUN_CONFIG="+configPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	output := &childOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// The stop must interrupt a RUNNING tunnel, so wait for the inbound to
	// answer before closing stdin.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for deadline := time.Now().Add(15 * time.Second); ; {
		conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child's inbound never came up; output:\n%s", output)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close child stdin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exited %v after stdin close, want a clean exit 0; output:\n%s", err, output)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("child ignored the stdin close; output:\n%s", output)
	}
}
