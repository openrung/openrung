package main

import (
	"context"
	"errors"
	"runtime"
)

// elevation implements connectcore.Elevation for the terminal client. A CLI
// cannot acquire privileges it was not started with — re-executing itself
// under sudo would detach the tunnel from the terminal that owns the TUI — so
// the hook verifies the platform's TUN preconditions and, when they are not
// met, refuses with what to do about it. That refusal is the engine's
// PREPARING-stage failure, so nothing is dialed and no telemetry session is
// opened.
//
// Each platform file owns both the check (tunModeAvailable) and the copy the
// Settings view and flag help show for TUN mode (tunModeSummary,
// tunModeAdvice), so the two can never disagree about what TUN costs here.
type elevation struct {
	// externalSingBox mirrors connectConfig.SingBoxExternal: the tunnel binary
	// is an explicit -sing-box, not the bundled runtime. On Windows that makes
	// TUN mode unstoppable and therefore refused (tunRequiresBundledRuntime).
	externalSingBox bool
}

func (e elevation) Elevate(context.Context) error {
	if err := tunRequiresBundledRuntime(runtime.GOOS, e.externalSingBox); err != nil {
		return err
	}
	return tunModeAvailable()
}

// tunRequiresBundledRuntime refuses TUN mode with an external -sing-box binary
// on Windows. The only graceful-stop channel there is the bundled runtime's
// stdin-close protocol (connectcore's client.StopOnStdinCloseFlag) — no
// interrupt can be delivered on Windows — and an external binary does not
// speak it, so every stop would be a hard kill leaving the routes and DNS
// sing-box installed pointing at a tunnel that is gone. The goos parameter
// keeps the refusal (and its wording) testable from every platform.
func tunRequiresBundledRuntime(goos string, externalSingBox bool) error {
	if goos != "windows" || !externalSingBox {
		return nil
	}
	return errors.New(
		"TUN mode on Windows needs the bundled sing-box runtime: an external -sing-box binary cannot be stopped gracefully on Windows, and a forced stop can leave the routing table and DNS pointing at a tunnel that is gone. Drop -sing-box, or stay in proxy mode",
	)
}
