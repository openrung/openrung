package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	"openrung/internal/client"
	"openrung/internal/connectcore"
	"openrung/internal/relay"
)

// The headless subcommands are thin engine drivers (docs/adr/001 B1): check
// and config keep their historical fetch-and-select behavior for scripts, and
// connect -headless drives the shared engine the way the TUI does, minus the
// terminal UI.

const (
	defaultRelayLimit  = 5
	defaultRelayFamily = string(client.RelayFamilyAuto)
)

type commonConfig struct {
	BrokerURL  string
	Limit      int
	MTU        int
	Family     string
	RelayID    string
	RelayLabel string
}

func parseCommonFlags(name string, args []string) (commonConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfg := commonConfig{}
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return commonConfig{}, err
	}
	return cfg, nil
}

func addCommonFlags(fs *flag.FlagSet, cfg *commonConfig) {
	fs.StringVar(&cfg.BrokerURL, "broker", "http://localhost:8080", "broker base URL")
	fs.IntVar(&cfg.Limit, "limit", defaultRelayLimit, "relay candidate limit (check/config)")
	fs.IntVar(&cfg.MTU, "mtu", 0, "TUN MTU; defaults to sing-box config default (config)")
	fs.StringVar(&cfg.Family, "relay-family", defaultRelayFamily, "relay address family: auto, ipv4, or ipv6 (check/config)")
	fs.StringVar(&cfg.RelayID, "relay-id", "", "connect only to the relay with this exact broker relay id (e.g. relay_abc...)")
	fs.StringVar(&cfg.RelayLabel, "relay-label", "", "connect only to the relay(s) with this label")
}

func (cfg commonConfig) target() connectcore.RelayTarget {
	return connectcore.RelayTarget{RelayID: cfg.RelayID, Label: cfg.RelayLabel}
}

func runCheck(args []string) error {
	cfg, err := parseCommonFlags("check", args)
	if err != nil {
		return err
	}

	selected, _, err := fetchSelectedRelay(context.Background(), cfg)
	if err != nil {
		return err
	}
	printSelectedRelay(os.Stdout, selected)
	return nil
}

func runConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfg := commonConfig{}
	addCommonFlags(fs, &cfg)
	outPath := fs.String("out", "", "write generated sing-box config to this path; defaults to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selected, configJSON, err := fetchSelectedRelay(context.Background(), cfg)
	if err != nil {
		return err
	}

	if *outPath == "" || *outPath == "-" {
		fmt.Print(string(configJSON))
		return nil
	}
	if err := os.WriteFile(*outPath, configJSON, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(os.Stdout, "wrote sing-box config for relay %s to %s\n", selected.ID, *outPath)
	return nil
}

// runHeadlessConnect is the old non-interactive connect, now engine-backed:
// the engine runs the full candidate ladder (ranking, WSS fallback, punch,
// mid-session failover) in proxy mode, logs stream to stdout, and an interrupt
// disconnects cleanly. Telemetry is the engine's own session lifecycle.
func runHeadlessConnect(cfg connectConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for _, warning := range legacyFlagWarnings(cfg) {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}

	sink := newConsoleSink(os.Stdout)
	host := newEngineHost(sink, cfg.SingBoxPath)
	engine := host.engine
	engine.PunchEnabled = cfg.PunchEnabled
	engine.PunchURL = cfg.PunchURL
	engine.PunchInsecure = cfg.PunchInsecure
	engine.Start()
	// Graceful teardown on every exit: tunnel down, OS proxy restored,
	// telemetry session ended and flushed.
	defer engine.Stop()

	if err := engine.ConnectTarget(cfg.BrokerURL, cfg.target()); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		stop() // restore default signal handling so a second interrupt force-kills
		fmt.Fprintln(os.Stdout, "interrupted; disconnecting")
		return nil
	case state := <-sink.terminal:
		if state.Status == connectcore.StatusFailed {
			if state.LastError != nil {
				return errors.New(*state.LastError)
			}
			return errors.New("connect failed")
		}
		return nil
	}
}

// consoleSink adapts engine events to the old CLI's line-oriented output:
// every log line prints as it arrives, status transitions print once, and
// terminal statuses (failed; disconnected without our interrupt) are handed to
// the driver loop. Like every engine sink it must not call back into the
// engine (see connectcore.EventSink).
type consoleSink struct {
	out io.Writer

	mu       sync.Mutex
	last     connectcore.Status
	terminal chan connectcore.State
}

func newConsoleSink(out io.Writer) *consoleSink {
	return &consoleSink{
		out:      out,
		last:     connectcore.StatusDisconnected,
		terminal: make(chan connectcore.State, 1),
	}
}

func (s *consoleSink) StateChanged(state connectcore.State) {
	s.mu.Lock()
	changed := state.Status != s.last
	s.last = state.Status
	s.mu.Unlock()
	if changed {
		line := "status: " + string(state.Status)
		if state.Status == connectcore.StatusConnected && state.RelayLabel != nil {
			line += " via " + *state.RelayLabel
		}
		fmt.Fprintln(s.out, line)
	}
	if state.Status == connectcore.StatusFailed || state.Status == connectcore.StatusDisconnected {
		select {
		case s.terminal <- state:
		default: // the driver already has a terminal state to act on
		}
	}
}

func (s *consoleSink) Log(entry connectcore.LogEntry) {
	fmt.Fprintf(s.out, "[%s] %s\n", entry.Time.Format("15:04:05"), entry.Line)
}

// fetchSelectedRelay fetches relay candidates and selects one for the
// fetch-and-print subcommands. It sends no identity headers: check and config
// begin no telemetry session, exactly as before the rewrite.
func fetchSelectedRelay(ctx context.Context, cfg commonConfig) (relay.Descriptor, []byte, error) {
	broker := client.BrokerClient{BaseURL: cfg.BrokerURL}
	// When pinning a specific relay, fetch the full candidate set so the target
	// isn't ranked out of a small -limit window.
	target := cfg.target()
	limit := cfg.Limit
	if target.Targeted() {
		limit = connectcore.DirectoryRelayLimit
	}
	resp, err := broker.ListRelays(ctx, limit, "", "")
	if err != nil {
		return relay.Descriptor{}, nil, err
	}

	matched, _, err := connectcore.FilterCandidates(resp.Relays, target)
	if err != nil {
		return relay.Descriptor{}, nil, err
	}
	resp.Relays = matched

	family, err := client.ParseRelayFamily(cfg.Family)
	if err != nil {
		return relay.Descriptor{}, nil, err
	}

	selected, err := client.SelectRelayForFamily(resp, family)
	if err != nil {
		if errors.Is(err, client.ErrNoUsableRelay) {
			return relay.Descriptor{}, nil, fmt.Errorf("no usable relay returned by broker")
		}
		return relay.Descriptor{}, nil, err
	}

	configJSON, err := client.BuildSingBoxConfig(client.SingBoxConfigInput{Relay: selected, MTU: cfg.MTU})
	if err != nil {
		return relay.Descriptor{}, nil, err
	}
	return selected, configJSON, nil
}

func printSelectedRelay(out io.Writer, selected relay.Descriptor) {
	expires := selected.ExpiresAt.Format(time.RFC3339)
	fmt.Fprintf(
		out,
		"selected relay %s at %s:%d, expires %s\n",
		selected.ID,
		selected.PublicHost,
		selected.PublicPort,
		expires,
	)
}
