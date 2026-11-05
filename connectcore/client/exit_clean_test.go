// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ExitedCleanly is what lets a fake-IP cache keep its block across runs, so it
// must be true only for a stop Stop asked for, finished in the graceful phase
// with a zero status.
func TestSingBoxProcessExitedCleanly(t *testing.T) {
	loop := "while :; do sleep 0.05; done\n"
	cases := []struct {
		name  string
		stub  string
		grace time.Duration
		stop  bool
		want  bool
	}{
		{"graceful-zero", "trap 'exit 0' INT\n" + loop, 5 * time.Second, true, true},
		{"graceful-nonzero", "trap 'exit 3' INT\n" + loop, 5 * time.Second, true, false},
		{"killed-after-grace", "trap '' INT\n" + loop, 200 * time.Millisecond, true, false},
		{"exited-unasked", "exit 0\n", 5 * time.Second, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "stubbox")
			ready := filepath.Join(dir, "ready")
			stub := tc.stub
			if tc.stop {
				// Signal readiness only once the trap is installed.
				stub = strings.Replace(stub, "\n", "\n: > "+ready+"\n", 1)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+stub), 0o755); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "config.json")
			if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			process, err := SingBoxRunner{Path: script}.Start(config)
			if err != nil {
				t.Fatal(err)
			}
			if process.ExitedCleanly() && tc.stop {
				t.Fatal("ExitedCleanly true while the process runs")
			}
			if tc.stop {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("stub never became ready")
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				select {
				case <-process.Done():
				case <-time.After(10 * time.Second):
					t.Fatal("process never exited")
				}
			}
			_ = process.Stop(tc.grace)
			if got := process.ExitedCleanly(); got != tc.want {
				t.Fatalf("ExitedCleanly = %v, want %v", got, tc.want)
			}
		})
	}
}
