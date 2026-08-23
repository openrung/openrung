package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
