//go:build !windows

package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// syncBuffer is a concurrency-safe writer: exec copies stdout and stderr on
// separate goroutines, exactly like the engine's two logWriter values.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunKillGraceBoundsCancelTeardown proves a cancelled Run returns promptly
// even when the child ignores the interrupt: the configurable grace (used by
// the desktop connect ladder) falls back to a hard kill.
func TestRunKillGraceBoundsCancelTeardown(t *testing.T) {
	dir := t.TempDir()
	// A stand-in "sing-box" that ignores SIGINT; only the post-grace hard kill
	// can end it. It receives "run -c <config>" like the real binary.
	script := filepath.Join(dir, "stubbox")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' INT\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	runner := SingBoxRunner{Path: script, KillGrace: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, config) }()

	time.Sleep(100 * time.Millisecond) // let the child start
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled run should return nil, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancel; KillGrace not honored")
	}
}

// TestRunCrashErrorQuotesTheChildsOwnWords proves an abnormal exit carries the
// child's error line, not just "exit status 1": that line is what reaches the
// user-facing Error row, and a bare exit status once hid a missing with_utls
// build tag the child had spelled out on stderr.
func TestRunCrashErrorQuotesTheChildsOwnWords(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\n" +
		"echo 'INFO some benign startup chatter'\n" +
		"echo 'error: create sing-box service: uTLS is not included in this build' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var logged syncBuffer
	runner := SingBoxRunner{Path: script, Stdout: &logged, Stderr: &logged}
	err := runner.Run(context.Background(), config)
	if err == nil {
		t.Fatal("crashing run returned nil")
	}
	for _, want := range []string{"sing-box exited", "uTLS is not included in this build", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("crash error %q missing %q", err, want)
		}
	}
	// The tee must still deliver the output to the engine log writers.
	if !strings.Contains(logged.String(), "uTLS is not included") || !strings.Contains(logged.String(), "benign startup chatter") {
		t.Fatalf("recorder swallowed output meant for the log: %q", logged.String())
	}
}

// A crash with no error-looking line still quotes the last thing the child
// said — even without a trailing newline.
func TestRunCrashErrorFallsBackToLastLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'giving up on relay handshake' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SingBoxRunner{Path: script}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "giving up on relay handshake") {
		t.Fatalf("crash error did not quote the unterminated last line: %v", err)
	}
}

// The quoted crash line is for user-facing surfaces only: the error must also
// offer a telemetry-safe rendering that carries the exit fact without the
// child's words, which can name local paths and usernames.
func TestCrashErrorOffersTelemetrySafeRendering(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\necho 'error: read config /Users/alice/secret.json: denied' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SingBoxRunner{Path: script}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "/Users/alice/secret.json") {
		t.Fatalf("user-facing error lost the quote: %v", err)
	}
	safer, ok := err.(interface{ TelemetrySafe() string })
	if !ok {
		t.Fatalf("crash error %T offers no telemetry-safe rendering", err)
	}
	safe := safer.TelemetrySafe()
	if strings.Contains(safe, "alice") || strings.Contains(safe, "secret") {
		t.Fatalf("telemetry rendering leaks the quote: %q", safe)
	}
	if !strings.Contains(safe, "sing-box exited") || !strings.Contains(safe, "exit status 1") {
		t.Fatalf("telemetry rendering lost the exit fact: %q", safe)
	}
}

// A crash quote must come from the child's FINAL lines: an error-looking line
// from long before the exit is more likely a recovered transient than the
// cause, and quoting it misdirects diagnosis.
func TestRunCrashQuoteIgnoresStaleErrorLines(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\n" +
		"echo 'ERROR transient handshake, retrying' >&2\n" +
		"i=0; while [ $i -lt 20 ]; do echo \"steady line $i\" >&2; i=$((i+1)); done\n" +
		"echo 'missing required field \"server\"' >&2\n" +
		"exit 2\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SingBoxRunner{Path: script}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "missing required field") {
		t.Fatalf("crash error did not quote the final message: %v", err)
	}
	if strings.Contains(err.Error(), "transient handshake") {
		t.Fatalf("crash error quoted a stale recovered error: %v", err)
	}
}

// os/exec serializes Write calls only while cmd.Stdout == cmd.Stderr, so a
// caller passing one plain unsynchronized writer for both streams must keep
// that guarantee through the recorder wrapping. The race detector is the
// assertion here.
func TestRunSameWriterKeepsExecSerialization(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\ni=0; while [ $i -lt 50 ]; do echo \"out $i\"; echo \"err $i\" >&2; i=$((i+1)); done\nexit 0\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf strings.Builder // deliberately unsynchronized
	err := SingBoxRunner{Path: script, Stdout: &buf, Stderr: &buf}.Run(context.Background(), config)
	if err == nil || err.Error() != "sing-box exited" {
		t.Fatalf("clean exit reported %v", err)
	}
	if !strings.Contains(buf.String(), "out 49") || !strings.Contains(buf.String(), "err 49") {
		t.Fatalf("shared writer missing output: %q", buf.String())
	}
}

// Even in shared-writer mode, each stream assembles its own lines: a partial
// stderr line interleaved with stdout output must come back whole, not
// spliced into cross-stream mush like "stderr failurestdout chatter".
func TestRunSharedWriterKeepsStreamsLineSeparate(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\n" +
		"printf 'error: real stderr failure' >&2\n" +
		"echo 'benign stdout chatter'\n" +
		"sleep 0.1\n" + // let both pipes drain so the interleave is real
		"printf ' continues\\n' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf strings.Builder
	err := SingBoxRunner{Path: script, Stdout: &buf, Stderr: &buf}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "real stderr failure continues") {
		t.Fatalf("stderr line not assembled across the interleave: %v", err)
	}
	if strings.Contains(err.Error(), "chatter") {
		t.Fatalf("quote spliced stdout into a stderr line: %v", err)
	}
}

// A forced flush of an over-long line must not split a multi-byte rune: the
// dangling bytes would put mojibake into the user-facing quote.
func TestCrashLineRecorderKeepsRuneBoundaries(t *testing.T) {
	r := &crashLineRecorder{}
	long := strings.Repeat("界", 300) // 900 bytes; the 512-byte cap lands mid-rune
	if _, err := r.Write([]byte("error: " + long + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := r.tailLines()
	if len(tail) == 0 {
		t.Fatal("recorder recorded nothing")
	}
	for _, line := range tail {
		if !utf8.ValidString(line.text) {
			t.Fatalf("recorded line is not valid UTF-8: %q", line.text)
		}
	}
}

// An error line on ANY stream outranks another stream's benign chatter:
// stderr small talk must not shadow a fatal report that went to stdout.
func TestRunCrashErrorPrefersStdoutErrorOverStderrChatter(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubbox")
	stub := "#!/bin/sh\n" +
		"echo 'shutting down cleanly' >&2\n" +
		"echo 'FATAL: actual startup failure'\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SingBoxRunner{Path: script}.Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "FATAL: actual startup failure") {
		t.Fatalf("crash error did not surface the stdout error line: %v", err)
	}
	if strings.Contains(err.Error(), "shutting down cleanly") {
		t.Fatalf("crash error quoted stderr chatter over the real error: %v", err)
	}
}
