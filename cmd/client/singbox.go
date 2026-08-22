package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"openrung/internal/singboxruntime"
)

// runSingBox is the internal `run` subcommand: the process face of the
// bundled sing-box runtime. connectcore's SingBoxRunner supervises the tunnel
// as a child process it invokes as `<binary> run -c <config>`, so pointing
// the engine's SingBoxPath at our own executable makes the client re-exec
// itself into this shim — the engine's interrupt/kill-grace teardown, process
// grouping, and crash isolation all stay exactly as they are with an external
// sing-box binary.
//
// SIGINT (the runner's cancellation interrupt, sent to the process group) and
// SIGTERM cancel the context; the runtime then closes the instance so a TUN
// tunnel unwinds its routes and DNS before exit, like upstream sing-box run.
// On Windows the runner cannot deliver an interrupt and hard-kills after the
// grace period, which is why TUN mode stays refused there (elevation_windows.go).
func runSingBox(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("c", "", "path to the sing-box config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("run: -c <config> is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return singboxruntime.Run(ctx, *configPath)
}

// bundledSingBoxPath resolves this executable, which doubles as the sing-box
// binary through the run subcommand above.
func bundledSingBoxPath() (string, error) {
	return os.Executable()
}
