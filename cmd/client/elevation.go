package main

import "context"

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
type elevation struct{}

func (elevation) Elevate(context.Context) error { return tunModeAvailable() }
