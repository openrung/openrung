package main

import "errors"

const (
	tunModeSummary = "unavailable on Windows"
	tunModeAdvice  = "TUN mode is not supported on Windows yet, so connecting will be refused — stay in proxy mode"
)

// tunModeAvailable refuses TUN mode on Windows regardless of the process
// token. Elevation is not the blocker; graceful teardown is.
//
// A TUN candidate is only safe to stop if sing-box gets to remove the routes
// and DNS settings it installed, and this client has no way to ask it to on
// Windows. os.Interrupt is unsupported there (internal/client's runner
// documents this, and Go returns "not supported by windows"), and the console
// control events that would substitute cannot reach a child started with
// CREATE_NO_WINDOW, which has no console to receive them. Every disconnect,
// quit, and failed ladder rung would therefore end in a force-kill, leaving
// the host routing traffic at an interface that no longer exists — the exact
// outcome connectcore.TUNKillGrace exists to avoid.
//
// Proxy mode is unaffected: it holds no device and no routes, so a hard kill
// there costs nothing. Lift this once the runner can stop sing-box gracefully
// on Windows.
func tunModeAvailable() error {
	return errors.New(
		"TUN mode is not supported on Windows yet: the client cannot stop sing-box gracefully here, and a forced stop can leave the routing table and DNS pointing at a tunnel that is gone. Use proxy mode (the default), which needs no privileges",
	)
}
