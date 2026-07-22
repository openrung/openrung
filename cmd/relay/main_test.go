package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

const testIdentitySeed = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="

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
	for _, required := range []string{"cmd/wsssidecar/**", "internal/wssbridge/**"} {
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

// testPreparedRuntime supplies the identity key prepareRuntime would have
// generated; tests construct preparedRuntime directly and bypass it.
func testPreparedRuntime(t *testing.T) preparedRuntime {
	t.Helper()
	identityKey, err := relay.ParseIdentitySeed(testIdentitySeed)
	if err != nil {
		t.Fatalf("parse identity seed: %v", err)
	}
	return preparedRuntime{IdentityKey: identityKey}
}

func TestVersionInfoAndReportedVersion(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = " 1.2.3 ", " abcdef0 "
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	if got := reportedRelayVersion(); got != "relay/1.2.3" {
		t.Fatalf("reportedRelayVersion() = %q, want relay/1.2.3", got)
	}
	if got := versionInfo(); got != "relay/1.2.3 revision=abcdef0" {
		t.Fatalf("versionInfo() = %q, want component, version, and revision", got)
	}
}

func TestHeartbeatOrRegisterRecoversForgottenRelay(t *testing.T) {
	var registrations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/relays/relay_old/heartbeat":
			return jsonResponse(http.StatusNotFound, `{"error":"relay not found"}`), nil
		case "/api/v1/relays/register":
			registrations.Add(1)
			return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{"error":"unexpected path"}`), nil
		}
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client}
	broker := cfg.brokerClient()
	desc, reRegistered, err := heartbeatOrRegister(context.Background(), broker, cfg, testPreparedRuntime(t), relay.Descriptor{ID: "relay_old"})
	if err != nil {
		t.Fatalf("heartbeatOrRegister() error = %v", err)
	}
	if !reRegistered {
		t.Fatal("heartbeatOrRegister() did not report re-registration")
	}
	if desc.ID != "relay_new" {
		t.Fatalf("heartbeatOrRegister() ID = %q, want relay_new", desc.ID)
	}
	if registrations.Load() != 1 {
		t.Fatalf("registrations = %d, want 1", registrations.Load())
	}
}

func TestHeartbeatOrRegisterDoesNotRegisterOnOtherErrors(t *testing.T) {
	var registrations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/relays/register" {
			registrations.Add(1)
		}
		return jsonResponse(http.StatusInternalServerError, `{"error":"temporary failure"}`), nil
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", HTTPClient: client}
	broker := cfg.brokerClient()
	original := relay.Descriptor{ID: "relay_old"}
	desc, reRegistered, err := heartbeatOrRegister(context.Background(), broker, cfg, testPreparedRuntime(t), original)
	if err == nil {
		t.Fatal("heartbeatOrRegister() error = nil, want an error")
	}
	if reRegistered {
		t.Fatal("heartbeatOrRegister() unexpectedly reported re-registration")
	}
	if desc.ID != original.ID {
		t.Fatalf("heartbeatOrRegister() ID = %q, want %q", desc.ID, original.ID)
	}
	if registrations.Load() != 0 {
		t.Fatalf("registrations = %d, want 0", registrations.Load())
	}
}

func TestTunnelModeRequiresHub(t *testing.T) {
	cfg := cliConfig{TunnelMode: true, MaxSessions: 1, MaxMbps: 1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when hub missing in tunnel mode")
	}

	cfg.HubAddr = "hub.example:9443"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected tunnel config to validate: %v", err)
	}
}

func TestValidateFoundationRequiresDirectMode(t *testing.T) {
	base := cliConfig{
		BrokerURL:         "https://broker.openrung.org",
		NodeClass:         relay.NodeClassFoundation,
		HubAddr:           "hub.example:9443",
		ListenPort:        443,
		PublicHost:        "relay.example",
		PublicPort:        443,
		MaxSessions:       1,
		MaxMbps:           1,
		HeartbeatInterval: 5 * time.Second,
		ListenHost:        "0.0.0.0",
		ConnectionLog:     false,
	}

	for _, mode := range []string{"auto", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			cfg := base
			cfg.Mode = mode
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want foundation %s rejection", mode)
			}
			if !strings.Contains(err.Error(), "requires direct mode") {
				t.Fatalf("Validate() error = %v, want direct-mode explanation", err)
			}
		})
	}

	direct := base
	direct.Mode = "direct"
	if err := direct.Validate(); err != nil {
		t.Fatalf("Validate() rejected foundation direct mode: %v", err)
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

func TestValidateWSSFrontsRequiresFoundationLocal443AndStableIdentity(t *testing.T) {
	fronts, err := parseWSSFrontsFlag("front-a=wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge")
	if err != nil {
		t.Fatal(err)
	}
	base := cliConfig{
		BrokerURL:         "https://broker.openrung.org",
		NodeClass:         relay.NodeClassFoundation,
		Mode:              "direct",
		ListenPort:        443,
		PublicHost:        "relay.example",
		PublicPort:        443,
		IdentitySeed:      testIdentitySeed,
		WSSFronts:         fronts,
		MaxSessions:       1,
		MaxMbps:           1,
		HeartbeatInterval: 5 * time.Second,
		ListenHost:        "0.0.0.0",
		ConnectionLog:     false,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid WSS relay config rejected: %v", err)
	}
	for _, listenHost := range []string{"", "0.0.0.0", "127.0.0.1"} {
		t.Run("IPv4 loopback listener "+listenHost, func(t *testing.T) {
			cfg := base
			cfg.ListenHost = listenHost
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() rejected IPv4-loopback-reachable listen-host %q: %v", listenHost, err)
			}
		})
	}

	for name, mutate := range map[string]func(*cliConfig){
		"volunteer":           func(cfg *cliConfig) { cfg.NodeClass = relay.NodeClassVolunteer },
		"auto":                func(cfg *cliConfig) { cfg.Mode, cfg.HubAddr = "auto", "hub.example:9443" },
		"public port":         func(cfg *cliConfig) { cfg.PublicPort = 8443 },
		"listener port":       func(cfg *cliConfig) { cfg.ListenPort = 8443 },
		"public-IP listener":  func(cfg *cliConfig) { cfg.ListenHost = "203.0.113.7" },
		"IPv6-only listener":  func(cfg *cliConfig) { cfg.ListenHost = "::1" },
		"hostname listener":   func(cfg *cliConfig) { cfg.ListenHost = "relay.example" },
		"IPv6 wildcard":       func(cfg *cliConfig) { cfg.ListenHost = "::" },
		"dual observer":       func(cfg *cliConfig) { cfg.ListenHost, cfg.ConnectionLog = "dual", true },
		"connection observer": func(cfg *cliConfig) { cfg.ConnectionLog = true },
		"ephemeral identity":  func(cfg *cliConfig) { cfg.IdentitySeed = "" },
		"invalid identity":    func(cfg *cliConfig) { cfg.IdentitySeed = "not-base64" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want WSS posture rejection")
			}
		})
	}
}

// run has its own fail-closed guard so a programmatic caller cannot bypass
// Validate and send the foundation token in auto mode's hub probe.
func TestRunRejectsFoundationAutoBeforeProbe(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	cfg := cliConfig{
		Mode:              "auto",
		NodeClass:         " Foundation ",
		HubAddr:           "hub.example:9443",
		RegistrationToken: "foundation-secret",
		HTTPClient:        client,
	}

	err := run(cfg)
	if err == nil {
		t.Fatal("run() error = nil, want foundation auto-mode rejection")
	}
	if requests.Load() != 0 {
		t.Fatalf("run() sent %d requests, want 0", requests.Load())
	}
}

func TestTunnelModeSkipsPublicHostDetection(t *testing.T) {
	cfg := cliConfig{TunnelMode: true, HubAddr: "hub.example:9443"}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults in tunnel mode: %v", err)
	}
	if cfg.PublicHost != "" {
		t.Fatalf("expected no public host in tunnel mode, got %q", cfg.PublicHost)
	}
	if cfg.Label == "" {
		t.Fatal("expected a generated label in tunnel mode")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A broker that predates node_class silently drops the field and returns a
// descriptor without it; a foundation relay must refuse to serve mislabeled
// rather than silently run as a volunteer-class relay.
func TestRegisterRejectsUnattestedFoundationClass(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443}`), nil
	})}

	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	if _, err := register(context.Background(), cfg.brokerClient(), cfg, testPreparedRuntime(t)); err == nil {
		t.Fatal("register() error = nil, want an unattested-node-class error")
	}
}

func TestRegisterAcceptsAttestedFoundationClass(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","public_host":"relay.example","public_port":443,"node_class":"foundation"}`), nil
	})}

	cfg := cliConfig{BrokerURL: "https://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	desc, err := register(context.Background(), cfg.brokerClient(), cfg, testPreparedRuntime(t))
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want foundation", desc.NodeClass)
	}
}

func TestRegisterSignsAndVerifiesExactPerRelayWSSFronts(t *testing.T) {
	prepared := testPreparedRuntime(t)
	expectedRelayID := relay.DeriveRelayID(prepared.IdentityKey.Public().(ed25519.PublicKey))
	var received relay.RegisterRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		if err := relay.VerifyWSSCapability(received, time.Now()); err != nil {
			t.Fatalf("broker-side capability verification failed: %v", err)
		}
		response, err := json.Marshal(relay.RegisterResponse{Descriptor: relay.Descriptor{
			ID:        expectedRelayID,
			NodeClass: relay.NodeClassFoundation,
			WSSFronts: slices.Clone(received.WSSFronts),
		}})
		if err != nil {
			t.Fatalf("marshal registration response: %v", err)
		}
		return jsonResponse(http.StatusCreated, string(response)), nil
	})}

	cfg := cliConfig{
		BrokerURL:     "https://broker.test",
		PublicHost:    "relay.example",
		PublicPort:    443,
		ListenPort:    443,
		NodeClass:     relay.NodeClassFoundation,
		IdentitySeed:  testIdentitySeed,
		WSSFrontsRaw:  "front-b=wss://cdn-b.example/api/v1/wss-bridge,front-a=wss://cdn-a.example/api/v1/wss-bridge",
		ListenHost:    "0.0.0.0",
		ConnectionLog: false,
		HTTPClient:    client,
	}
	desc, err := register(context.Background(), cfg.brokerClient(), cfg, prepared)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.ID != expectedRelayID || len(desc.WSSFronts) != 2 {
		t.Fatalf("descriptor = %+v, want stable relay ID and two exact fronts", desc)
	}
	if received.WSSCapabilityProof == "" || received.WSSCapabilityExpiresAt != received.IdentityExpiresAt {
		t.Fatalf("capability proof fields = proof:%t expiry:%q identity-expiry:%q", received.WSSCapabilityProof != "", received.WSSCapabilityExpiresAt, received.IdentityExpiresAt)
	}
	if received.WSSFronts[0].ID != "front-a" || received.WSSFronts[1].ID != "front-b" {
		t.Fatalf("registration fronts not canonical: %#v", received.WSSFronts)
	}
}

func TestRegisterRejectsBrokerThatDropsOrRewritesWSSCapability(t *testing.T) {
	prepared := testPreparedRuntime(t)
	expectedRelayID := relay.DeriveRelayID(prepared.IdentityKey.Public().(ed25519.PublicKey))
	for name, responseDescriptor := range map[string]relay.Descriptor{
		"dropped fronts": {
			ID: expectedRelayID, NodeClass: relay.NodeClassFoundation,
		},
		"wrong relay ID": {
			ID: "relay_wrong", NodeClass: relay.NodeClassFoundation,
			WSSFronts: []relay.WSSFrontDescriptor{{ID: "front-a", URL: "wss://cdn-a.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion}},
		},
		"rewritten front": {
			ID: expectedRelayID, NodeClass: relay.NodeClassFoundation,
			WSSFronts: []relay.WSSFrontDescriptor{{ID: "front-a", URL: "wss://other.example/api/v1/wss-bridge", ProtocolVersion: relay.WSSProtocolVersion}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := json.Marshal(relay.RegisterResponse{Descriptor: responseDescriptor})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusCreated, string(response)), nil
			})}
			cfg := cliConfig{
				BrokerURL: "https://broker.test", PublicHost: "relay.example",
				PublicPort: 443, ListenPort: 443, NodeClass: relay.NodeClassFoundation,
				IdentitySeed:  testIdentitySeed,
				WSSFrontsRaw:  "front-a=wss://cdn-a.example/api/v1/wss-bridge",
				ListenHost:    "0.0.0.0",
				ConnectionLog: false,
				HTTPClient:    client,
			}
			if _, err := register(context.Background(), cfg.brokerClient(), cfg, prepared); err == nil {
				t.Fatal("register() error = nil, want fail-closed broker echo rejection")
			}
		})
	}
}

// register() must refuse a foundation registration over a cleartext broker URL
// before it ever sends the token (enforced by BrokerClient.RequireSecureTransport,
// which brokerClient() sets for foundation).
func TestRegisterRefusesFoundationOverPlaintext(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusCreated, `{"id":"relay_new","node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client, NodeClass: relay.NodeClassFoundation}
	if _, err := register(context.Background(), cfg.brokerClient(), cfg, testPreparedRuntime(t)); err == nil {
		t.Fatal("register() error = nil, want a cleartext-broker error")
	}
	if sent.Load() != 0 {
		t.Fatalf("register sent %d requests, want 0 (must refuse before sending the token)", sent.Load())
	}
}

// Heartbeats carry the same foundation bearer as registration, so the secure
// transport policy must cover them too.
func TestHeartbeatRefusesFoundationOverPlaintext(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	cfg := cliConfig{
		BrokerURL:         "http://broker.test",
		RegistrationToken: "foundation-secret",
		HTTPClient:        client,
		NodeClass:         relay.NodeClassFoundation,
	}
	if err := heartbeat(context.Background(), cfg.brokerClient(), "relay_foundation", ""); err == nil {
		t.Fatal("heartbeat() error = nil, want a cleartext-broker error")
	}
	if sent.Load() != 0 {
		t.Fatalf("heartbeat sent %d requests, want 0", sent.Load())
	}
}

// A foundation token is a self-contained posture: setting only
// OPENRUNG_FOUNDATION_TOKEN (no OPENRUNG_NODE_CLASS) must register as
// foundation, over the foundation bearer, on an https broker.
func TestFoundationTokenRegistersAsFoundationWithoutNodeClass(t *testing.T) {
	var gotAuth, gotBody string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}
		return jsonResponse(http.StatusCreated, `{"id":"relay_x","public_host":"relay.example","public_port":443,"node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{
		BrokerURL:       "https://broker.test",
		FoundationToken: "fnd-secret",
		PublicHost:      "relay.example",
		PublicPort:      443,
		HTTPClient:      client,
	}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if cfg.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("node_class = %q, want foundation (forced by the token)", cfg.NodeClass)
	}
	if cfg.Mode != "direct" {
		t.Fatalf("mode = %q, want direct (forced by the token)", cfg.Mode)
	}
	desc, err := register(context.Background(), cfg.brokerClient(), cfg, testPreparedRuntime(t))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.NodeClass != relay.NodeClassFoundation {
		t.Fatalf("attested node_class = %q, want foundation", desc.NodeClass)
	}
	if gotAuth != "Bearer fnd-secret" {
		t.Fatalf("Authorization = %q, want the foundation token as bearer", gotAuth)
	}
	if !strings.Contains(gotBody, `"node_class":"foundation"`) {
		t.Fatalf("register body did not claim foundation: %s", gotBody)
	}
}

// A hub var would normally resolve to auto; a foundation token overrides that
// to direct so the token can never route through a hub.
func TestFoundationTokenForcesDirectOverResolvedAuto(t *testing.T) {
	cfg := cliConfig{
		FoundationToken: "fnd-secret",
		BrokerURL:       "https://broker.test",
		PublicHost:      "relay.example",
		HubAddr:         "hub.example:9443",
	}
	cfg.Mode = normalizeMode(cfg.Mode, cfg.TunnelMode, cfg.HubAddr) // main() does this first
	if cfg.Mode != "auto" {
		t.Fatalf("precondition: mode = %q, want auto (hub implies auto)", cfg.Mode)
	}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if cfg.NodeClass != relay.NodeClassFoundation || cfg.Mode != "direct" {
		t.Fatalf("posture = %q/%q, want foundation/direct", cfg.NodeClass, cfg.Mode)
	}
}

func TestFoundationTokenConflictsWithExplicitVolunteerClass(t *testing.T) {
	cfg := cliConfig{FoundationToken: "fnd-secret", NodeClass: "volunteer", BrokerURL: "https://broker.test", PublicHost: "relay.example"}
	if err := cfg.ApplyDefaults(); err == nil {
		t.Fatal("ApplyDefaults() error = nil, want a node-class conflict error")
	}
}

func TestFoundationTokenIsBearerAndForcesSecureTransport(t *testing.T) {
	cfg := cliConfig{FoundationToken: "fnd-secret", RegistrationToken: "vol-token", BrokerURL: "https://broker.test"}
	bc := cfg.brokerClient()
	if bc.Token != "fnd-secret" {
		t.Fatalf("bearer = %q, want the foundation token (not the volunteer token)", bc.Token)
	}
	if !bc.RequireSecureTransport {
		t.Fatal("RequireSecureTransport = false, want true for a foundation token")
	}
}

// The token forces secure transport intrinsically: a foundation token against a
// cleartext broker is refused before any request is sent.
func TestFoundationTokenRefusesPlaintextBroker(t *testing.T) {
	var sent atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return jsonResponse(http.StatusCreated, `{"node_class":"foundation"}`), nil
	})}
	cfg := cliConfig{FoundationToken: "fnd-secret", BrokerURL: "http://broker.test", PublicHost: "relay.example", PublicPort: 443, HTTPClient: client}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if _, err := register(context.Background(), cfg.brokerClient(), cfg, testPreparedRuntime(t)); err == nil {
		t.Fatal("register() error = nil, want a cleartext-broker refusal")
	}
	if sent.Load() != 0 {
		t.Fatalf("register sent %d requests, want 0 (must refuse before sending)", sent.Load())
	}
}
