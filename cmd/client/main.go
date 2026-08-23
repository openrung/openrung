// Command client is the OpenRung end-user terminal client (ADR-001
// Track B): an interactive TUI over the shared connection engine in the
// nested connectcore module — the same engine the desktop GUI drives — plus
// thin headless subcommands for scripts. The view layer holds no connection
// logic: engine events in, engine commands out.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/proxyconfig"
	"openrung/internal/buildinfo"
	"openrung/internal/singboxruntime"
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
	// The bundled sing-box engine's process face: the engine's runner
	// re-execs this binary into it (internal/singboxruntime).
	case singboxruntime.Subcommand:
		return singboxruntime.RunSubcommand(args[1:])
	case "-v", "-version", "--version", "version":
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
	return buildinfo.Info("client", baseVersion) + "\n" + singboxruntime.VersionLine()
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
	TUN           bool
}

// mode is the capture mode the flags select. Proxy mode is the default, as on
// the desktop app; --tun asks for full-device capture and is refused without
// the privileges to create the tunnel device.
func (cfg connectConfig) mode() connectcore.Mode {
	if cfg.TUN {
		return connectcore.ModeTUN
	}
	return connectcore.ModeProxy
}

func runConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	cfg := connectConfig{}
	addCommonFlags(fs, &cfg.commonConfig)
	fs.StringVar(&cfg.SingBoxPath, "sing-box", "", "path to an external sing-box binary (default: the bundled sing-box runtime)")
	fs.StringVar(&cfg.ConfigOut, "config-out", "", "deprecated: the engine manages its own config; use the config subcommand")
	fs.BoolVar(&cfg.PunchEnabled, "punch", true, "attempt a direct NAT-punched path before falling back to the relay")
	fs.StringVar(&cfg.PunchURL, "punch-url", "", "override the hub punch coordinator base URL (else use the relay's advertised punch_endpoint)")
	fs.BoolVar(&cfg.PunchInsecure, "punch-insecure", false, "skip TLS verification of the hub punch API (for a self-signed hub cert; testing)")
	fs.BoolVar(&cfg.Headless, "headless", false, "non-interactive connect: stream engine logs to stdout until interrupted")
	fs.BoolVar(&cfg.TUN, "tun", false, "route the whole device through a TUN interface instead of the local proxy ("+tunModeSummary+")")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The empty default selects the bundled sing-box: the engine's runner
	// re-execs this binary into the internal run subcommand. An explicit
	// -sing-box keeps its historical external-binary meaning.
	if cfg.SingBoxPath == "" {
		exe, err := singboxruntime.SelfPath()
		if err != nil {
			return fmt.Errorf("%w (pass -sing-box to use an external binary)", err)
		}
		cfg.SingBoxPath = exe
	}

	if cfg.Headless {
		return runHeadlessConnect(cfg)
	}
	return runTUI(cfg)
}

// legacyFlagWarnings names the still-parsed connect flags the shared engine
// deliberately no longer honors, so old scripts keep running and say why their
// knob went quiet. The engine owns candidate paging, walks its own ladder over
// every address family, and manages its temp config lifecycle.
func legacyFlagWarnings(cfg connectConfig) []string {
	var warnings []string
	if cfg.Limit != defaultRelayLimit {
		warnings = append(warnings, "-limit is ignored: the connection engine manages the relay candidate page size")
	}
	if cfg.MTU != 0 && !cfg.TUN {
		warnings = append(warnings, "-mtu applies to the TUN device only: this session starts in proxy mode (pass --tun, or use the config subcommand)")
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
	return fmt.Errorf("expected one of: check, config, connect, run")
}

func printUsage() {
	program := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `Usage:
  %[1]s                    Launch the interactive terminal client.
  %[1]s connect            Same as bare invocation; flags seed the initial settings.
  %[1]s connect --tun      Full-device routing instead of the local proxy (%[2]s).
  %[1]s connect -headless  Non-interactive connect (old behavior, engine-backed).
  %[1]s check   -broker http://localhost:8080
  %[1]s config  -broker http://localhost:8080 -out openrung-sing-box.json

Commands:
  connect  Interactive TUI by default: Status, Relays, Logs, and Settings views
           over the shared connection engine. With -headless, connect and
           stream logs until interrupted.
  check    Fetch relay candidates and print the selected usable relay.
  config   Generate a sing-box TUN client config for the selected relay.
  run      Internal: run the bundled sing-box with -c <config>. The connect
           engine invokes it on this binary; it also works standalone in
           place of "sing-box run -c <config>".
  version  Print the client and bundled sing-box versions and exit (-v).

Keys (interactive):
  c connect  d disconnect  r refresh relays  1-4/tab switch view  q quit

Connect flags:
  --tun           Capture the whole device through a TUN interface (%[2]s).
                  Settings can also toggle it while disconnected. Without it
                  the client runs the zero-privilege proxy mode: a loopback
                  mixed HTTP/SOCKS inbound with the OS proxy pointed at it.
  -sing-box       Path to an external sing-box binary. By default the client
                  runs its bundled sing-box; no separate install is needed.
  -punch          Attempt a direct NAT-punched path before the relay hub.

Common flags:
  -broker         Broker base URL override (e.g. http://localhost:8080 for a
                  local broker). Empty (the default) races the built-in HTTPS
                  broker fronts and uses the first that answers.
  -relay-id       Pin the connect target to this exact broker relay id.
  -relay-label    Pin the connect target to relay(s) with this label.
  -mtu            Override the TUN MTU (connect --tun, and the config
                  subcommand).
  -relay-family   Select relay family for check/config: auto, ipv4, or ipv6.

`, program, tunModeSummary)
}
