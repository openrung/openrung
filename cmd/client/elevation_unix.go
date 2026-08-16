//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// tunModeSummary is the parenthetical the Settings Mode row and the --tun
	// flag help carry; tunModeAdvice is the sentence shown after switching to
	// TUN mode.
	tunModeSummary = "needs sudo"
	tunModeAdvice  = "the client must be running under sudo"
)

// tunModeAvailable reports whether this process may create a TUN device and
// install routes. The check is deliberately the effective uid rather than an
// inspection of Linux capabilities: sudo is the documented and portable way to
// run the client in TUN mode, and refusing a CAP_NET_ADMIN-only process costs
// a rerun, while wrongly admitting one would surface as an opaque sing-box
// failure after the ladder had already dialed relays.
//
// The rerun command uses the name the user actually invoked, so it matches an
// installed binary and a `go run` build alike.
func tunModeAvailable() error {
	if os.Geteuid() == 0 {
		return nil
	}
	return fmt.Errorf(
		"TUN mode needs root privileges to create the tunnel device: rerun as `sudo %s connect --tun`, or drop --tun to use proxy mode (no privileges needed)",
		filepath.Base(os.Args[0]),
	)
}
