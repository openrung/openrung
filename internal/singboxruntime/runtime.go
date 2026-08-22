// Package singboxruntime embeds the sing-box tunnel engine as a Go library so
// the terminal client ships as one self-contained binary with no separate
// sing-box install. It is the in-process body behind the client's internal
// `run` subcommand: connectcore's SingBoxRunner keeps supervising the tunnel
// as a child process (`<client> run -c <config>`), and that child lands here.
//
// Reality/uTLS support is compiled in only with the `with_utls` build tag
// (see Makefile and .github/workflows/client-release.yml); every OpenRung
// relay endpoint needs it, and a build without the tag fails at tunnel
// creation with upstream's "rebuild with -tags with_utls" error.
package singboxruntime

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

const modulePath = "github.com/sagernet/sing-box"

// Load parses the sing-box config at configPath and constructs (but does not
// start) the service, mirroring upstream's create step. The returned context
// carries the protocol registries and must be the one the instance runs
// under. Split from Run so tests can validate every generated config shape
// without privileges or a network.
func Load(ctx context.Context, configPath string) (*box.Box, context.Context, error) {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read sing-box config: %w", err)
	}
	ctx = include.Context(ctx)
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, content)
	if err != nil {
		return nil, nil, fmt.Errorf("decode sing-box config %s: %w", configPath, err)
	}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		return nil, nil, fmt.Errorf("create sing-box service: %w", err)
	}
	return instance, ctx, nil
}

// Run starts the embedded sing-box with the config at configPath and blocks
// until ctx is cancelled, then closes the instance — the same cancel-then-
// close order as upstream's run command, so a TUN instance unwinds its routes
// and DNS before the process exits.
func Run(ctx context.Context, configPath string) error {
	// A child cancel scopes the instance: on a failed start the deferred
	// cancel is the cleanup, matching upstream (no Close on a never-started
	// instance).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	instance, ctx, err := Load(ctx, configPath)
	if err != nil {
		return err
	}
	if err := instance.Start(); err != nil {
		return fmt.Errorf("start sing-box service: %w", err)
	}
	<-ctx.Done()
	if err := instance.Close(); err != nil {
		return fmt.Errorf("close sing-box service: %w", err)
	}
	return nil
}

// Version reports the bundled sing-box module version (e.g.
// "v1.14.0-beta.17"), read from the binary's build info. "unknown" only in
// `go test` binaries, which carry no dependency list; the client release
// workflow smoke-tests the real binary's version output instead.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			if dep.Replace != nil {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}
