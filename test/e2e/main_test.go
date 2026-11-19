// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

func TestMain(m *testing.M) {
	if os.Getenv("LAB_TRACE") != "" {
		lab.Trace = func(line string) { fmt.Fprintln(os.Stderr, "+ "+line) }
	}
	if err := lab.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "lab did not come up:", err)
		dumpLogs(os.Stderr, "broker", "hub", "relay-a", "relay-b", "vol", "internet", "dns")
		os.Exit(1)
	}
	code := m.Run()
	lab.Down()
	os.Exit(code)
}

// scenario marks the start of a scenario: when it ends, the client is
// stopped and the lab is returned to its fresh state, so no scenario's
// network or stopped services leak into the next.
func scenario(t *testing.T) {
	t.Helper()
	logStart := lab.LogSize("client")
	t.Cleanup(func() {
		if t.Failed() {
			log, _ := lab.ReadLog("client", logStart)
			t.Logf("client log:\n%s", plain(log))
		}
		if err := lab.Stop("client"); err != nil {
			t.Errorf("stop client: %v", err)
		}
		if err := lab.Restore(); err != nil {
			t.Fatalf("restore lab: %v", err)
		}
	})
}

// slow skips a scenario that waits on real client timers (health checks,
// leases) when the suite runs with -test.short.
func slow(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow scenario skipped in short mode")
	}
}

// needNetem skips a scenario that needs added latency on a kernel without
// the netem qdisc.
func needNetem(t *testing.T) {
	t.Helper()
	if !lab.HasNetem() {
		t.Skip(lab.ErrNoNetem)
	}
}

// connect starts the client and waits until it reports a working tunnel.
func connect(t *testing.T, args ...string) (*lab.Client, string) {
	t.Helper()
	client := startClient(t, args...)
	via, err := client.WaitConnected(90 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, via
}

func startClient(t *testing.T, args ...string) *lab.Client {
	t.Helper()
	must(t, lab.Stop("client"))
	client, err := lab.StartClient(args...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// ansi matches the colour codes the tunnel runtime writes to its log.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(log string) string { return ansi.ReplaceAllString(log, "") }

// exitIP asserts where the internet sees the client's traffic coming from.
func exitIP(t *testing.T, client *lab.Client, want string) {
	t.Helper()
	got, err := client.ExitIP(20 * time.Second)
	if err != nil {
		t.Fatalf("fetch through the tunnel: %v", err)
	}
	if got != want {
		t.Fatalf("traffic exits at %s, want %s", got, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func dumpLogs(w *os.File, services ...string) {
	for _, name := range services {
		log, _ := lab.ReadLog(name, 0)
		lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}
		fmt.Fprintf(w, "=== %s ===\n%s\n", name, strings.Join(lines, "\n"))
	}
}
