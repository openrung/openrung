package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// elevation implements connectcore.Elevation for the terminal client. A CLI
// cannot acquire privileges it was not started with — re-executing itself
// under sudo would detach the tunnel from the terminal that owns the TUI — so
// the hook verifies them and, when they are missing, refuses with the exact
// command to rerun. That refusal is the engine's PREPARING-stage failure, so
// nothing is dialed and no telemetry session is opened.
type elevation struct{}

func (elevation) Elevate(context.Context) error {
	if elevated() {
		return nil
	}
	return fmt.Errorf("TUN mode needs %s to create the tunnel device: %s, or drop --tun to use proxy mode (no privileges needed)",
		elevationPrivilege, elevationHint())
}

// elevationHint spells out the rerun command using the name the user actually
// invoked, so it matches an installed binary and a `go run` build alike.
func elevationHint() string {
	program := filepath.Base(os.Args[0])
	return fmt.Sprintf(elevationRerunFormat, program)
}
