package singboxruntime

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/openrung/openrung/connectcore/client"
)

// Subcommand is the argv verb connectcore's SingBoxRunner invokes on the
// binary it is given: it runs `<binary> run -c <config>`. Both hosts of the
// bundled runtime — the terminal client and the desktop app — dispatch this
// verb before any of their own startup work, which is what lets them pass
// their own executable as the engine's SingBoxPath.
const Subcommand = "run"

// RunSubcommand is the process body behind that verb. The engine's
// supervision — interrupt-then-kill grace, process grouping, crash isolation,
// tunnel-death detection by exit status — stays exactly as it is with an
// external sing-box binary, so a nonzero exit on failure is part of the
// contract (cmd/client's TestRunSubcommandExitsNonzeroOnStartFailure).
//
// Two stop channels cancel the context, and the runtime then closes the
// instance so a TUN tunnel unwinds its routes and DNS before exit, like
// upstream sing-box run:
//
//   - SIGINT (the runner's cancellation interrupt, sent to the process group)
//     and SIGTERM. Signals are Unix-only in practice — neither can be
//     delivered on Windows.
//   - stdin reaching EOF, when the runner opted in with the
//     -stop-on-stdin-close flag (client.StopOnStdinCloseFlag names it, so the
//     two sides cannot drift). The runner holds our stdin pipe open for our
//     lifetime and closes it to stop us; the OS also closes it if the runner
//     dies without ever running teardown. This is the stop channel that works
//     on Windows, and the flag opt-in keeps a standalone
//     `<binary> run -c <config> </dev/null` from exiting at launch.
func RunSubcommand(args []string) error {
	fs := flag.NewFlagSet(Subcommand, flag.ContinueOnError)
	configPath := fs.String("c", "", "path to the sing-box config file")
	stopOnStdinClose := fs.Bool(client.StopOnStdinCloseFlag, false,
		"treat stdin reaching EOF as a stop request (the connect engine's stop channel)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New(Subcommand + ": -c <config> is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stopOnStdinClose {
		go watchStdinClose(os.Stdin, stop)
	}
	return Run(ctx, *configPath)
}

// watchStdinClose blocks until r is exhausted or fails, then calls stop. Any
// return means the other end is gone or done with us — a deliberate close
// (EOF), or a broken pipe from the runner's death — and both mean the same
// thing: stop the tunnel while it can still unwind its routes and DNS.
func watchStdinClose(r io.Reader, stop func()) {
	_, _ = io.Copy(io.Discard, r)
	stop()
}

// SelfPath resolves the running executable, which doubles as the sing-box
// binary through the run subcommand above. It is what both hosts assign to the
// engine's SingBoxPath.
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable for the bundled sing-box runtime: %w", err)
	}
	return exe, nil
}

// VersionLine names the bundled engine and the build shape that decides
// whether it can dial a relay at all: without the with_utls tag the runtime
// fails Reality configs with upstream's rebuild hint, and this label is the
// first place to look when a build fails at tunnel creation. Both hosts pin
// this exact line against the go.mod version before shipping — the client in
// client-release.yml, the desktop app in
// desktop/scripts/verify-bundled-engine.mjs — so a build that loses the tag
// fails instead of reaching users unable to connect.
func VersionLine() string {
	return fmt.Sprintf("sing-box/%s (bundled, %s)", strings.TrimPrefix(Version(), "v"), utlsBuildLabel())
}

func utlsBuildLabel() string {
	if UTLSEnabled {
		return "with_utls"
	}
	return "NO with_utls — cannot dial Reality relays; rebuild with -tags with_utls"
}
