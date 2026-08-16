// Command client is the OpenRung end-user terminal client (docs/adr/001
// Track B): an interactive TUI over the shared connection engine in
// internal/connectcore — the same engine the desktop GUI drives — plus thin
// headless subcommands for scripts. The view layer holds no connection logic:
// engine events in, engine commands out.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"openrung/internal/buildinfo"
	"openrung/internal/proxyconfig"
)

//go:embed VERSION
var baseVersion string

func main() {
	// Must run before anything can dial the broker: launched from a shell the
	// generated proxy helper activated, our own bootstrap would otherwise be
	// sent through the not-yet-listening local endpoint.
	proxyconfig.SanitizeInheritedProxyEnvironment()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Bare invocation launches the interactive client, like the desktop app.
	if len(args) == 0 {
		return runConnect(nil)
	}

	switch args[0] {
	case "check":
		return runCheck(args[1:])
	case "config":
		return runConfig(args[1:])
	case "connect":
		return runConnect(args[1:])
	case "-version", "--version", "version":
		fmt.Println(versionInfo())
		return nil
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return usageError()
	}
}

func versionInfo() string {
	return buildinfo.Info("client", baseVersion)
}

// connectConfig is the connect subcommand's flag surface: the historical
// common flags plus the tunnel-run flags. The legacy flags that the engine now
// owns are still parsed (scripts pass them) and warned about instead of
// breaking; see legacyFlagWarnings.
type connectConfig struct {
	commonConfig
	SingBoxPath   string
	ConfigOut     string
	PunchEnabled  bool
	PunchURL      string
	PunchInsecure bool
	Headless      bool
}

func runConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	cfg := connectConfig{}
	addCommonFlags(fs, &cfg.commonConfig)
	fs.StringVar(&cfg.SingBoxPath, "sing-box", "sing-box", "path to sing-box binary")
	fs.StringVar(&cfg.ConfigOut, "config-out", "", "deprecated: the engine manages its own config; use the config subcommand")
	fs.BoolVar(&cfg.PunchEnabled, "punch", true, "attempt a direct NAT-punched path before falling back to the relay")
	fs.StringVar(&cfg.PunchURL, "punch-url", "", "override the hub punch coordinator base URL (else use the relay's advertised punch_endpoint)")
	fs.BoolVar(&cfg.PunchInsecure, "punch-insecure", false, "skip TLS verification of the hub punch API (for a self-signed hub cert; testing)")
	fs.BoolVar(&cfg.Headless, "headless", false, "non-interactive connect: stream engine logs to stdout until interrupted")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cfg.Headless {
		return runHeadlessConnect(cfg)
	}
	return runTUI(cfg)
}

// legacyFlagWarnings names the still-parsed connect flags the shared engine
// deliberately no longer honors, so old scripts keep running and say why their
// knob went quiet. The engine owns candidate paging, runs the zero-privilege
// proxy mode (no TUN MTU until ADR-001 B3), walks its own ladder over every
// address family, and manages its temp config lifecycle.
func legacyFlagWarnings(cfg connectConfig) []string {
	var warnings []string
	if cfg.Limit != defaultRelayLimit {
		warnings = append(warnings, "-limit is ignored: the connection engine manages the relay candidate page size")
	}
	if cfg.MTU != 0 {
		warnings = append(warnings, "-mtu is ignored: connect runs in proxy mode (TUN mode returns in a later release; the config subcommand still honors -mtu)")
	}
	if cfg.Family != defaultRelayFamily {
		warnings = append(warnings, "-relay-family is ignored by connect: the engine tries every usable relay (check and config still honor it)")
	}
	if cfg.ConfigOut != "" {
		warnings = append(warnings, "-config-out is ignored: the engine manages its own temp config; use the config subcommand to export one")
	}
	return warnings
}

func usageError() error {
	printUsage()
	return fmt.Errorf("expected one of: check, config, connect")
}

func printUsage() {
	program := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `Usage:
  %[1]s                    Launch the interactive terminal client.
  %[1]s connect            Same as bare invocation; flags seed the initial settings.
  %[1]s connect -headless  Non-interactive connect (old behavior, engine-backed).
  %[1]s check   -broker http://localhost:8080
  %[1]s config  -broker http://localhost:8080 -out openrung-sing-box.json

Commands:
  connect  Interactive TUI by default: Status, Relays, Logs, and Settings views
           over the shared connection engine (proxy mode, no privileges).
           With -headless, connect and stream logs until interrupted.
  check    Fetch relay candidates and print the selected usable relay.
  config   Generate a sing-box TUN client config for the selected relay.
  version  Print the client version and exit.

Keys (interactive):
  c connect  d disconnect  r refresh relays  1-4/tab switch view  q quit

Common flags:
  -broker         Broker base URL override (e.g. http://localhost:8080 for a
                  local broker). Empty (the default) races the built-in HTTPS
                  broker fronts and uses the first that answers.
  -relay-id       Pin the connect target to this exact broker relay id.
  -relay-label    Pin the connect target to relay(s) with this label.
  -mtu            Override the generated TUN MTU (config subcommand).
  -relay-family   Select relay family for check/config: auto, ipv4, or ipv6.

`, program)
}
