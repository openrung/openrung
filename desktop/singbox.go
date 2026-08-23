package main

import (
	"fmt"

	"github.com/openrung/openrung/connectcore/client"
	"openrung/internal/singboxruntime"
)

// dispatchSubcommand answers the small argv surface this GUI binary must serve
// before it can open a window.
//
// The app statically links the sing-box engine (internal/singboxruntime) and
// hands the connection engine its own executable as SingBoxPath, so
// connectcore's runner starts a tunnel by re-invoking this binary as
// `OpenRung run -c <config>`. That child is the sing-box process: it must
// never reach wails.Run, and its exit status is how the runner detects tunnel
// death. `version` prints the app version the packaging linker stamped in plus
// the linked engine's version and build tags; that is what
// scripts/verify-bundled-engine.mjs reads to refuse packaging a build which
// lost `with_utls` and would install fine and then fail every connect.
//
// The bool reports whether args were a subcommand; anything else — no
// arguments, or whatever a desktop launcher passes — starts the GUI.
func dispatchSubcommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case singboxruntime.Subcommand:
		return true, singboxruntime.RunSubcommand(args[1:])
	case "version", "-version", "--version":
		fmt.Printf("OpenRung/%s\n%s\n", client.AppVersion(), singboxruntime.VersionLine())
		return true, nil
	default:
		return false, nil
	}
}
