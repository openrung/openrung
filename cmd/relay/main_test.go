package main

import (
	"bytes"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
	"openrung/internal/relayruntime/engine"
)

const testIdentitySeed = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="

// relayEnvironmentVariables are every OPENRUNG_* variable the relay reads as a
// flag default. Tests clear them so a developer's shell cannot change what a
// flag defaults to.
var relayEnvironmentVariables = []string{
	"OPENRUNG_VOLUNTEER_TOKEN",
	"OPENRUNG_FOUNDATION_TOKEN",
	"OPENRUNG_LABEL",
	"OPENRUNG_NODE_CLASS",
	"OPENRUNG_WSS_FRONTS",
	"OPENRUNG_MODE",
	"OPENRUNG_TUNNEL",
	"OPENRUNG_HUB_ADDR",
	"OPENRUNG_HUB_HTTP_URL",
	"OPENRUNG_HUB_CERT_FINGERPRINT",
	"OPENRUNG_PUNCH_DISABLE",
}

// parseFlags runs the real registration and parsing path, so what these tests
// assert is exactly what main() and -h see.
func parseFlags(t *testing.T, args ...string) *cliFlags {
	t.Helper()
	for _, name := range relayEnvironmentVariables {
		t.Setenv(name, "")
	}
	flags := &cliFlags{}
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags.register(fs, "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return flags
}

// The CLI surface is a compatibility contract: deploy/relay/entrypoint.sh,
// volunteer-up.sh, foundation-up.sh, and foundation-wss-host.sh all invoke the
// binary with these names, so a rename or a changed default breaks a
// deployment that this repository cannot see. Flags may be added, never
// removed or redefined.
func TestFlagSurfaceAndDefaultsAreStable(t *testing.T) {
	for _, name := range relayEnvironmentVariables {
		t.Setenv(name, "")
	}
	want := map[string]string{
		"version":              "false",
		"print-label":          "false",
		"broker":               "http://localhost:8080",
		"registration-token":   "",
		"label":                "",
		"node-class":           "",
		"foundation-token":     "",
		"xray":                 "xray",
		"listen-host":          "::",
		"listen-port":          "443",
		"public-host":          "",
		"public-port":          "443",
		"server-name":          "www.cloudflare.com",
		"reality-dest":         "www.cloudflare.com:443",
		"client-id":            "",
		"reality-private-key":  "",
		"reality-public-key":   "",
		"short-id":             "",
		"identity-seed":        "",
		"wss-fronts":           "",
		"max-sessions":         strconv.Itoa(relayruntime.DefaultMaxSessions),
		"max-mbps":             strconv.Itoa(relayruntime.DefaultMaxMbps),
		"heartbeat-interval":   "30s",
		"config-out":           "",
		"connection-log":       "true",
		"print-config-only":    "false",
		"skip-xray-run":        "false",
		"mode":                 "",
		"tunnel":               "false",
		"hub":                  "",
		"hub-http":             "",
		"hub-cert-fingerprint": "",
		"hub-tls":              "true",
		"hub-insecure":         "false",
		"punch":                "true",
	}

	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	(&cliFlags{}).register(fs, "")
	got := map[string]string{}
	fs.VisitAll(func(f *flag.Flag) { got[f.Name] = f.DefValue })

	for name, wantDefault := range want {
		gotDefault, ok := got[name]
		if !ok {
			t.Errorf("flag -%s disappeared from the CLI surface", name)
			continue
		}
		if gotDefault != wantDefault {
			t.Errorf("flag -%s default = %q, want %q", name, gotDefault, wantDefault)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("flag -%s is new: add it to the contract list once it is deliberate", name)
		}
	}
}

// The direct-mode argv deploy/relay/entrypoint.sh builds must land on the
// engine intact — this is the whole job of the frontend.
func TestDeployedDirectFlagsMapOntoEngineConfig(t *testing.T) {
	flags := parseFlags(t,
		"-mode", "direct",
		"-xray", "/usr/local/bin/xray",
		"-config-out", "/tmp/openrung-xray-config.json",
		"-broker", "https://broker.example.com",
		"-listen-host", "0.0.0.0",
		"-listen-port", "8443",
		"-heartbeat-interval", "45s",
		"-connection-log=false",
		"-public-host", "203.0.113.7",
		"-public-port", "443",
		"-server-name", "www.example.com",
		"-reality-dest", "www.example.com:443",
		"-client-id", "b831381d-6324-4d53-ad4f-8cda48b30811",
		"-reality-private-key", "private-key",
		"-reality-public-key", "public-key",
		"-short-id", "0123456789abcdef",
		"-identity-seed", testIdentitySeed,
		"-max-sessions", "10",
		"-max-mbps", "20",
		"-label", "brisk-otter",
		"-node-class", "foundation",
		"-foundation-token", "fnd-secret",
		"-registration-token", "vol-secret",
	)
	cfg, err := flags.engineConfig()
	if err != nil {
		t.Fatalf("engineConfig: %v", err)
	}

	if cfg.Mode != engine.ModeDirect || cfg.BrokerURL != "https://broker.example.com" {
		t.Errorf("mode/broker = %q/%q", cfg.Mode, cfg.BrokerURL)
	}
	if cfg.XrayPath != "/usr/local/bin/xray" || cfg.ConfigPath != "/tmp/openrung-xray-config.json" {
		t.Errorf("xray/config-out = %q/%q", cfg.XrayPath, cfg.ConfigPath)
	}
	if cfg.ListenHost != "0.0.0.0" || cfg.ListenPort != 8443 {
		t.Errorf("listen = %s:%d", cfg.ListenHost, cfg.ListenPort)
	}
	if cfg.PublicHost != "203.0.113.7" || cfg.PublicPort != 443 {
		t.Errorf("public = %s:%d", cfg.PublicHost, cfg.PublicPort)
	}
	if cfg.HeartbeatInterval != 45*time.Second {
		t.Errorf("heartbeat interval = %s", cfg.HeartbeatInterval)
	}
	// -connection-log=false must leave the engine with nowhere to write
	// per-connection client addresses at all, not merely a quiet writer.
	if cfg.ConnectionLogOutput != nil {
		t.Error("connection-log=false still handed the engine a log writer")
	}
	if cfg.ServerName != "www.example.com" || cfg.RealityDest != "www.example.com:443" {
		t.Errorf("camouflage = %q/%q", cfg.ServerName, cfg.RealityDest)
	}
	wantIdentity := engine.Identity{
		ClientID:          "b831381d-6324-4d53-ad4f-8cda48b30811",
		RealityPrivateKey: "private-key",
		RealityPublicKey:  "public-key",
		ShortID:           "0123456789abcdef",
		IdentitySeed:      testIdentitySeed,
	}
	if cfg.Identity != wantIdentity {
		t.Errorf("identity = %+v, want %+v", cfg.Identity, wantIdentity)
	}
	if cfg.MaxSessions != 10 || cfg.MaxMbps != 20 || cfg.Label != "brisk-otter" {
		t.Errorf("capacity/label = %d/%d/%q", cfg.MaxSessions, cfg.MaxMbps, cfg.Label)
	}
	if cfg.NodeClass != "foundation" || cfg.FoundationToken != "fnd-secret" || cfg.Token != "vol-secret" {
		t.Errorf("credentials = %q/%q/%q", cfg.NodeClass, cfg.FoundationToken, cfg.Token)
	}
	if cfg.Version != reportedRelayVersion() {
		t.Errorf("version = %q, want %q", cfg.Version, reportedRelayVersion())
	}
}

// Tunnel/auto mode's hub flags, including the legacy -tunnel alias and the
// TLS posture the entrypoint passes through.
func TestDeployedHubFlagsMapOntoEngineConfig(t *testing.T) {
	flags := parseFlags(t,
		"-mode", "tunnel",
		"-hub", "hub.example:9443",
		"-hub-http", "https://hub.example:9444",
		"-hub-tls=true",
		"-hub-insecure=false",
	)
	cfg, err := flags.engineConfig()
	if err != nil {
		t.Fatalf("engineConfig: %v", err)
	}
	if cfg.Mode != engine.ModeTunnel || cfg.HubAddr != "hub.example:9443" || cfg.HubHTTPURL != "https://hub.example:9444" {
		t.Errorf("hub config = %q/%q/%q", cfg.Mode, cfg.HubAddr, cfg.HubHTTPURL)
	}
	if cfg.HubPlaintext || cfg.HubInsecure {
		t.Errorf("hub TLS posture = plaintext:%v insecure:%v, want a verified TLS dial", cfg.HubPlaintext, cfg.HubInsecure)
	}
	if !cfg.PunchCapable {
		t.Error("punching is offered by default and was dropped")
	}

	plaintext, err := parseFlags(t, "-tunnel", "-hub", "hub.example:9443", "-hub-tls=false", "-hub-insecure=true").engineConfig()
	if err != nil {
		t.Fatalf("engineConfig: %v", err)
	}
	if plaintext.Mode != engine.ModeTunnel {
		t.Errorf("legacy -tunnel resolved to %q, want tunnel", plaintext.Mode)
	}
	if !plaintext.HubPlaintext || !plaintext.HubInsecure {
		t.Errorf("hub TLS opt-outs = plaintext:%v insecure:%v, want both", plaintext.HubPlaintext, plaintext.HubInsecure)
	}
}

// Leaf-cert pinning is new to the CLI and must stay inert until a fingerprint
// is actually supplied, so no existing deployment changes how it trusts a hub.
func TestHubCertPinningIsOffUntilRequested(t *testing.T) {
	cfg, err := parseFlags(t, "-hub", "hub.example:9443").engineConfig()
	if err != nil {
		t.Fatalf("engineConfig: %v", err)
	}
	if cfg.HubCertFingerprint != "" {
		t.Fatalf("hub pin = %q with no flag set, want none", cfg.HubCertFingerprint)
	}

	pinned, err := parseFlags(t, "-hub", "hub.example:9443", "-hub-cert-fingerprint", "AB:CD").engineConfig()
	if err != nil {
		t.Fatalf("engineConfig: %v", err)
	}
	if pinned.HubCertFingerprint != "AB:CD" {
		t.Fatalf("hub pin = %q, want the flag value verbatim", pinned.HubCertFingerprint)
	}
}

// Auto mode is implied by a hub and never by the absence of one; -mode wins
// over both. (The engine rejects anything else.)
func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		mode       string
		tunnelFlag bool
		hub        string
		want       string
	}{
		{"", false, "", "direct"},               // no hub, no flags → direct (legacy default)
		{"", false, "hub:9443", "auto"},         // hub set → auto
		{"", true, "hub:9443", "tunnel"},        // -tunnel forces tunnel
		{"direct", false, "hub:9443", "direct"}, // explicit wins
		{"tunnel", false, "", "tunnel"},         // explicit tunnel
		{"AUTO", false, "hub:9443", "auto"},     // case-insensitive
		{"bogus", false, "", "bogus"},           // invalid passes through (the engine rejects)
	}
	for _, c := range cases {
		if got := normalizeMode(c.mode, c.tunnelFlag, c.hub); got != c.want {
			t.Errorf("normalizeMode(%q, %v, %q) = %q, want %q", c.mode, c.tunnelFlag, c.hub, got, c.want)
		}
	}
}

// A mistyped -identity-seed must fail the process, not silently mint a new
// relay identity: the engine heals a corrupt seed for the GUI, which for a
// server would churn the relay ID on every restart.
func TestMalformedFlagValuesAreRejected(t *testing.T) {
	for name, args := range map[string][]string{
		"identity seed": {"-identity-seed", "not-base64"},
		"wss fronts":    {"-wss-fronts", "front-a"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFlags(t, args...).engineConfig(); err == nil {
				t.Fatalf("engineConfig(%v) error = nil, want a rejection", args)
			}
		})
	}
}

// The posture rules live in the engine now; this checks the frontend really
// hands it the fields they key off, rather than swallowing them.
func TestPostureFlagsReachTheEngine(t *testing.T) {
	for name, args := range map[string][]string{
		"foundation class cannot use a hub":                 {"-node-class", "foundation", "-mode", "auto", "-hub", "hub.example:9443", "-broker", "https://broker.example"},
		"foundation token conflicts with a volunteer class": {"-foundation-token", "fnd", "-node-class", "volunteer", "-broker", "https://broker.example"},
		"wss fronts require foundation":                     {"-wss-fronts", "a=wss://cdn.example/api/v1/wss-bridge", "-broker", "https://broker.example"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseFlags(t, args...).engineConfig()
			if err != nil {
				t.Fatalf("engineConfig: %v", err)
			}
			if err := engine.New(cfg, engine.Events{}).Start(); err == nil {
				t.Fatal("Start() error = nil, want the engine to reject this posture")
			}
		})
	}
}

// deploy/relay/foundation-up.sh and foundation-wss-host.sh poll container logs
// for "registered relay" as the proof a candidate came up, so the frontend has
// to keep emitting it from the engine's status stream.
func TestConsoleReporterEmitsRegistrationLines(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	reporter := &consoleReporter{}
	reporter.observe(engine.Status{Phase: engine.PhaseRegistering})
	reporter.observe(engine.Status{
		Phase: engine.PhaseOnline, Transport: relay.TransportDirect,
		RelayID: "relay_1", Label: "brisk-otter", PublicHost: "203.0.113.7", PublicPort: 443,
	})
	if got := logged.String(); !strings.Contains(got, "registered relay") ||
		!strings.Contains(got, "id=relay_1") || !strings.Contains(got, "public=203.0.113.7:443") {
		t.Fatalf("registration line = %q, want the deployment-contract fields", got)
	}

	// A status repeat is not a second registration.
	logged.Reset()
	reporter.observe(engine.Status{
		Phase: engine.PhaseOnline, Transport: relay.TransportDirect,
		RelayID: "relay_1", Label: "brisk-otter", PublicHost: "203.0.113.7", PublicPort: 443,
	})
	if logged.Len() != 0 {
		t.Fatalf("unchanged online status logged %q", logged.String())
	}

	// A new lease within the same session reports as a re-registration — which
	// the deployment grep still matches.
	logged.Reset()
	reporter.observe(engine.Status{
		Phase: engine.PhaseOnline, Transport: relay.TransportDirect,
		RelayID: "relay_2", Label: "brisk-otter", PublicHost: "203.0.113.7", PublicPort: 443,
	})
	if got := logged.String(); !strings.Contains(got, "re-registered relay") || !strings.Contains(got, "registered relay") {
		t.Fatalf("re-registration line = %q", got)
	}

	logged.Reset()
	reporter.observe(engine.Status{Phase: engine.PhaseRetrying, LastError: "broker unreachable"})
	if got := logged.String(); !strings.Contains(got, "broker unreachable") {
		t.Fatalf("retry line = %q, want the failure surfaced", got)
	}

	logged.Reset()
	reporter.observe(engine.Status{
		Phase: engine.PhaseOnline, Transport: relay.TransportTunnel,
		RelayID: "relay_hub_1", PublicHost: "198.51.100.4", PublicPort: 20001,
	})
	if got := logged.String(); !strings.Contains(got, "relay published via hub") || !strings.Contains(got, "relay_id=relay_hub_1") {
		t.Fatalf("tunnel line = %q", got)
	}
}

func TestVersionInfoAndReportedVersion(t *testing.T) {
	// Injection and fallback resolution are internal/buildinfo's tests; this
	// guards the relay's wiring: its component name and embedded VERSION,
	// including the relay_version identity reported to the broker and hub.
	wantReported := "relay/" + strings.TrimSpace(baseVersion)
	if got := reportedRelayVersion(); got != wantReported {
		t.Fatalf("reportedRelayVersion() = %q, want %q", got, wantReported)
	}
	if got := versionInfo(); !strings.HasPrefix(got, wantReported+" revision=") {
		t.Fatalf("versionInfo() = %q, want prefix %q", got, wantReported+" revision=")
	}
}

func TestParseWSSFrontsFlagNormalizesAndSorts(t *testing.T) {
	fronts, err := parseWSSFrontsFlag(" Front-B = WSS://CDN-B.EXAMPLE/api/v1/wss-bridge ,front-a=wss://cdn-a.example/api/v1/wss-bridge")
	if err != nil {
		t.Fatalf("parseWSSFrontsFlag: %v", err)
	}
	want := []relay.WSSFrontDescriptor{
		{ID: "front-a", URL: "wss://cdn-a.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
		{ID: "front-b", URL: "wss://cdn-b.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion},
	}
	if !slices.Equal(fronts, want) {
		t.Fatalf("fronts = %#v, want %#v", fronts, want)
	}
	for _, raw := range []string{
		"front-a",
		"=wss://cdn.example/api/v1/wss-bridge",
		"front-a=",
		"front-a=wss://cdn.example/api/v1/wss-bridge,",
		"front-a=https://cdn.example/api/v1/wss-bridge",
	} {
		if _, err := parseWSSFrontsFlag(raw); err == nil {
			t.Errorf("parseWSSFrontsFlag(%q) error = nil, want rejection", raw)
		}
	}
}

func TestRelayEntrypointKeepsIdentitySeedOutOfArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relay container entrypoint requires a POSIX shell")
	}

	original, err := os.ReadFile(filepath.Join("..", "..", "deploy", "relay", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read relay entrypoint: %v", err)
	}

	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args")
	envPath := filepath.Join(tempDir, "identity-seed")
	fakeRelayPath := filepath.Join(tempDir, "relay")
	fakeRelay := `#!/bin/sh
printf '%s\n' "$@" > "$OPENRUNG_TEST_ARGS_OUT"
printf '%s' "${OPENRUNG_IDENTITY_SEED-}" > "$OPENRUNG_TEST_ENV_OUT"
`
	if err := os.WriteFile(fakeRelayPath, []byte(fakeRelay), 0o700); err != nil {
		t.Fatalf("write fake relay: %v", err)
	}

	entrypoint := strings.Replace(string(original), "/usr/local/bin/relay", fakeRelayPath, 1)
	if entrypoint == string(original) {
		t.Fatal("entrypoint relay path was not replaced")
	}
	entrypointPath := filepath.Join(tempDir, "entrypoint.sh")
	if err := os.WriteFile(entrypointPath, []byte(entrypoint), 0o700); err != nil {
		t.Fatalf("write test entrypoint: %v", err)
	}

	const seed = "test-long-lived-identity-seed"
	cmd := exec.Command("sh", entrypointPath)
	cmd.Env = append(os.Environ(),
		"OPENRUNG_MODE=tunnel",
		"OPENRUNG_HUB_ADDR=hub.example:9443",
		relayruntime.IdentitySeedEnvironmentVariable+"="+seed,
		"OPENRUNG_TEST_ARGS_OUT="+argsPath,
		"OPENRUNG_TEST_ENV_OUT="+envPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run relay entrypoint: %v\n%s", err, output)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read relay argv: %v", err)
	}
	if strings.Contains(string(args), "-identity-seed") || strings.Contains(string(args), seed) {
		t.Fatalf("entrypoint exposed identity seed in relay argv: %q", args)
	}
	inheritedSeed, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read relay environment capture: %v", err)
	}
	if string(inheritedSeed) != seed {
		t.Fatalf("relay received identity seed %q, want environment-provided seed", inheritedSeed)
	}
}

func TestRelayEntrypointPassesPerRelayWSSFronts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relay container entrypoint requires a POSIX shell")
	}
	original, err := os.ReadFile(filepath.Join("..", "..", "deploy", "relay", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read relay entrypoint: %v", err)
	}
	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args")
	fakeRelayPath := filepath.Join(tempDir, "relay")
	fakeRelay := `#!/bin/sh
printf '%s\n' "$@" > "$OPENRUNG_TEST_ARGS_OUT"
`
	if err := os.WriteFile(fakeRelayPath, []byte(fakeRelay), 0o700); err != nil {
		t.Fatalf("write fake relay: %v", err)
	}
	entrypoint := strings.Replace(string(original), "/usr/local/bin/relay", fakeRelayPath, 1)
	entrypointPath := filepath.Join(tempDir, "entrypoint.sh")
	if err := os.WriteFile(entrypointPath, []byte(entrypoint), 0o700); err != nil {
		t.Fatalf("write test entrypoint: %v", err)
	}

	const fronts = "front-a=wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge,front-b=wss://cdn.example.org/api/v1/wss-bridge"
	cmd := exec.Command("sh", entrypointPath)
	cmd.Env = append(os.Environ(),
		"OPENRUNG_MODE=direct",
		"OPENRUNG_BROKER_URL=https://broker.example",
		"OPENRUNG_PUBLIC_HOST=relay.example",
		"OPENRUNG_WSS_FRONTS="+fronts,
		"OPENRUNG_TEST_ARGS_OUT="+argsPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run relay entrypoint: %v\n%s", err, output)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read relay argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	index := slices.Index(lines, "-wss-fronts")
	if index < 0 || index+1 >= len(lines) || lines[index+1] != fronts {
		t.Fatalf("entrypoint WSS args = %q, want -wss-fronts followed by exact per-relay fronts", lines)
	}
}

// Every flag deploy/relay/entrypoint.sh can emit must still exist, so a
// container built from this tree keeps starting.
func TestEntrypointFlagsAllExist(t *testing.T) {
	entrypoint, err := os.ReadFile(filepath.Join("..", "..", "deploy", "relay", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read relay entrypoint: %v", err)
	}
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	(&cliFlags{}).register(fs, "")

	// The relay argv is only ever built by `set -- …`, so reading the tokens
	// after that keeps the shell's own options (set -eu, [ -n … ]) out. Fold
	// line continuations first: the argv spans several lines.
	script := strings.ReplaceAll(string(entrypoint), "\\\n", " ")
	found := 0
	for _, line := range strings.Split(script, "\n") {
		_, args, ok := strings.Cut(line, "set -- ")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(args) {
			// Bool flags are written -name=value; the rest take a separate
			// argument.
			name, _, _ := strings.Cut(strings.Trim(field, `"`), "=")
			if !strings.HasPrefix(name, "-") {
				continue
			}
			found++
			if fs.Lookup(strings.TrimPrefix(name, "-")) == nil {
				t.Errorf("deploy/relay/entrypoint.sh passes %s, which the relay no longer defines", name)
			}
		}
	}
	if found == 0 {
		t.Fatal("found no relay flags in the entrypoint; the scan no longer matches how it builds argv")
	}
}

func TestRelayDeploymentCoLocatesHardenedWSSSidecar(t *testing.T) {
	deployDir := filepath.Join("..", "..", "deploy", "relay")
	read := func(name string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(deployDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(contents)
	}

	dockerfile := read("Dockerfile")
	for _, required := range []string{
		"go build -trimpath -ldflags=\"$ldflags\" -o /out/wss-sidecar ./cmd/wsssidecar",
		"COPY --from=build /out/wss-sidecar /usr/local/bin/wss-sidecar",
		"chown openrung:openrung /var/lib/openrung",
		"chmod 0700 /var/lib/openrung",
		"setcap 'cap_net_bind_service=+ep' /usr/local/bin/relay",
		"setcap 'cap_net_bind_service=+ep' /usr/local/bin/xray",
		"getcap /usr/local/bin/xray | grep -q 'cap_net_bind_service=ep'",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("relay Dockerfile does not bundle sidecar: missing %q", required)
		}
	}

	compose := read("docker-compose.yml")
	for _, required := range []string{
		"wss-sidecar:",
		"profiles: [wss]",
		"image: openrung-relay:latest",
		"entrypoint: [/usr/local/bin/wss-sidecar]",
		"path: .wss.env",
		"network_mode: host",
		"cap_drop: [ALL]",
		"no-new-privileges:true",
		"read_only: true",
		"127.0.0.1:443",
		"wss-replay-state:/var/lib/openrung",
		"wss-replay-state:",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("relay compose sidecar wiring missing %q", required)
		}
	}
	if strings.Contains(compose, "OPENRUNG_WSS_FIXED_TARGET:") || strings.Contains(compose, "OPENRUNG_WSS_FIXED_TARGET=") {
		t.Fatal("relay compose makes the sidecar's fixed localhost Reality target configurable")
	}
	const replayMount = "wss-replay-state:/var/lib/openrung"
	sidecarStart := strings.Index(compose, "\n  wss-sidecar:")
	topLevelVolumes := strings.LastIndex(compose, "\nvolumes:")
	if sidecarStart < 0 || topLevelVolumes <= sidecarStart {
		t.Fatal("relay compose service/top-level volume structure is malformed")
	}
	if !strings.Contains(compose[sidecarStart:topLevelVolumes], replayMount) {
		t.Fatalf("WSS sidecar does not own durable replay mount %q", replayMount)
	}
	if strings.Contains(compose[:sidecarStart], replayMount) {
		t.Fatal("durable replay state was mounted into the Reality relay instead of only the sidecar")
	}
	if !strings.Contains(compose[topLevelVolumes:], "\n  wss-replay-state:") {
		t.Fatal("relay compose does not declare its relay-local replay volume")
	}

	relayEnv := read(".env.example")
	if !strings.Contains(relayEnv, "OPENRUNG_WSS_FRONTS=") || !strings.Contains(relayEnv, "OPENRUNG_IDENTITY_SEED=") {
		t.Fatal("relay env example does not wire per-relay fronts to an explicit stable identity")
	}
	sidecarEnv := read(".wss.env.example")
	for _, required := range []string{
		"OPENRUNG_WSS_RELAY_ID=",
		"OPENRUNG_WSS_TICKET_PUBLIC_KEYS=",
		"OPENRUNG_WSS_FRONT_ORIGIN_TOKENS=",
		"OPENRUNG_WSS_REPLAY_STATE=/var/lib/openrung/wss-replay.journal",
	} {
		if !strings.Contains(sidecarEnv, required) {
			t.Errorf("sidecar env example missing %q", required)
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "deploy", "wssgateway")); !os.IsNotExist(err) {
		t.Fatalf("standalone WSS gateway deployment exists; sidecars must remain per-relay (stat error %v)", err)
	}
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "relay-image.yml"))
	if err != nil {
		t.Fatalf("read relay image workflow: %v", err)
	}
	for _, required := range []string{"cmd/wsssidecar/**", "internal/wssbridge/**", "brokerapi/go.mod", "wsscore/**"} {
		if !strings.Contains(string(workflow), required) {
			t.Errorf("relay image workflow will not rebuild for %q changes", required)
		}
	}
	readme := read("README.md")
	for _, required := range []string{
		"docker compose --profile wss up -d --build",
		"wss-replay-state",
		"cloudfront-wss.md",
		"foundation-up.sh` currently manages and rolls only the single Reality relay",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("relay deployment README is missing WSS coordination guidance %q", required)
		}
	}
}
