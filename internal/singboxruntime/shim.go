package singboxruntime

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
// SIGINT (the runner's cancellation interrupt, sent to the process group) and
// SIGTERM cancel the context; the runtime then closes the instance so a TUN
// tunnel unwinds its routes and DNS before exit, like upstream sing-box run.
// On Windows the runner cannot deliver an interrupt and hard-kills after the
// grace period, which is why TUN mode stays refused there
// (cmd/client/elevation_windows.go) and the desktop app is proxy-mode only.
func RunSubcommand(args []string) error {
	fs := flag.NewFlagSet(Subcommand, flag.ContinueOnError)
	configPath := fs.String("c", "", "path to the sing-box config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New(Subcommand + ": -c <config> is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, *configPath)
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
