package relayruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFakeXrayAPI installs a shell stand-in for `xray api` that records its
// argv and the user fragment it was handed, then prints scripted output.
func writeFakeXrayAPI(t *testing.T, dir, output string) (path, argvLog, fragmentLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake xray requires a POSIX shell")
	}
	path = filepath.Join(dir, "xray")
	argvLog = filepath.Join(dir, "argv")
	fragmentLog = filepath.Join(dir, "fragment")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
for arg in "$@"; do
  case "$arg" in
    *.json) cat "$arg" > %q ;;
  esac
done
printf '%%s\n' %q
`, argvLog, fragmentLog, output)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}
	return path, argvLog, fragmentLog
}

func TestXrayAPIAddUserBuildsFragmentAndReadsOutcome(t *testing.T) {
	dir := t.TempDir()
	path, argvLog, fragmentLog := writeFakeXrayAPI(t, dir, "processing inbound: vless-reality-in\nadd user: cred-x\nresult: ok\nAdded 1 user(s) in total.")
	api := &XrayAPI{Path: path, Addr: "127.0.0.1:10085", Flow: "xtls-rprx-vision", Dir: dir}
	credential := Credential{ID: "11111111-2222-4333-8444-555555555555", Email: "cred-20260917T140000Z"}
	if err := api.AddUser(context.Background(), credential); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	args := strings.Fields(string(argv))
	if len(args) != 5 || args[0] != "api" || args[1] != "adu" || args[2] != "-s" || args[3] != "127.0.0.1:10085" || !strings.HasSuffix(args[4], ".json") {
		t.Fatalf("argv = %q", args)
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatalf("the user fragment %s must be removed after the call (stat err = %v)", args[4], err)
	}
	fragment, err := os.ReadFile(fragmentLog)
	if err != nil {
		t.Fatalf("read fragment: %v", err)
	}
	var decoded struct {
		Inbounds []struct {
			Tag      string `json:"tag"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			Settings struct {
				Decryption string `json:"decryption"`
				Clients    []struct {
					ID    string `json:"id"`
					Email string `json:"email"`
					Flow  string `json:"flow"`
				} `json:"clients"`
			} `json:"settings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(fragment, &decoded); err != nil {
		t.Fatalf("fragment is not JSON: %v: %s", err, fragment)
	}
	if len(decoded.Inbounds) != 1 {
		t.Fatalf("fragment inbounds = %d, want 1", len(decoded.Inbounds))
	}
	in := decoded.Inbounds[0]
	if in.Tag != XrayInboundTag || in.Protocol != "vless" || in.Port == 0 || in.Settings.Decryption != "none" {
		t.Fatalf("fragment inbound = %+v", in)
	}
	if len(in.Settings.Clients) != 1 || in.Settings.Clients[0].ID != credential.ID || in.Settings.Clients[0].Email != credential.Email || in.Settings.Clients[0].Flow != "xtls-rprx-vision" {
		t.Fatalf("fragment clients = %+v", in.Settings.Clients)
	}
}

// `xray api` exits 0 even when nothing was added or removed, so the wrapper
// must read the outcome from the output: a duplicate add and an unknown
// remove are converged states, anything else is a failure.
func TestXrayAPIOutcomesFromOutput(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		call    func(*XrayAPI) error
		wantErr bool
	}{
		{"add already exists", "add user: cred-x\nrpc error: code = Unknown desc = proxy/vless: User cred-x already exists.\nAdded 0 user(s) in total.", func(a *XrayAPI) error {
			return a.AddUser(context.Background(), Credential{ID: "id", Email: "cred-x"})
		}, false},
		{"add rejected", "processing inbound: vless-reality-in\nfailed to build config: infra/conf: bad\nAdded 0 user(s) in total.", func(a *XrayAPI) error {
			return a.AddUser(context.Background(), Credential{ID: "id", Email: "cred-x"})
		}, true},
		{"remove ok", "remove user: cred-x\nRemoved 1 user(s) in total.", func(a *XrayAPI) error {
			return a.RemoveUser(context.Background(), "cred-x")
		}, false},
		{"remove unknown", "remove user: cred-x\nrpc error: code = Unknown desc = proxy/vless: User cred-x not found.\nRemoved 0 user(s) in total.", func(a *XrayAPI) error {
			return a.RemoveUser(context.Background(), "cred-x")
		}, false},
		{"remove failed", "remove user: cred-x\nrpc error: code = Unavailable desc = boom\nRemoved 0 user(s) in total.", func(a *XrayAPI) error {
			return a.RemoveUser(context.Background(), "cred-x")
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path, _, _ := writeFakeXrayAPI(t, dir, test.output)
			api := &XrayAPI{Path: path, Addr: "127.0.0.1:1", Flow: "xtls-rprx-vision", Dir: dir}
			err := test.call(api)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestXrayAPIRemoveUserArgv(t *testing.T) {
	dir := t.TempDir()
	path, argvLog, _ := writeFakeXrayAPI(t, dir, "Removed 1 user(s) in total.")
	api := &XrayAPI{Path: path, Addr: "127.0.0.1:10085", Dir: dir}
	if err := api.RemoveUser(context.Background(), "cred-20260917T140000Z"); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	argv, _ := os.ReadFile(argvLog)
	if got := strings.Fields(string(argv)); strings.Join(got, " ") != "api rmu -s 127.0.0.1:10085 -tag vless-reality-in cred-20260917T140000Z" {
		t.Fatalf("argv = %q", got)
	}
}

// Against the real binary: a relay config rendered with the management API
// and no static client admits nobody until a user is added at runtime, the
// added user is listed, adds and removes are idempotent, and a removed user
// is gone. Runs only where xray is installed (CI hosts skip).
func TestXrayAPIManagesUsersOnRunningXray(t *testing.T) {
	xrayPath := requireXray(t)
	keys, err := GenerateRealityKeyPair(xrayPath)
	if err != nil {
		t.Fatalf("generate Reality keys: %v", err)
	}
	listenPort := freeLoopbackPort(t)
	apiPort := freeLoopbackPort(t)
	config, err := BuildXrayConfig(XrayConfigInput{
		ListenHost:        "127.0.0.1",
		ListenPort:        listenPort,
		APIPort:           apiPort,
		Flow:              "xtls-rprx-vision",
		Dest:              "www.cloudflare.com:443",
		ServerName:        "www.cloudflare.com",
		RealityPrivateKey: keys.PrivateKey,
		ShortID:           "0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "xray.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := NewXrayCommand(ctx, xrayPath, "run", "-config", configPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start xray: %v", err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	api := &XrayAPI{Path: xrayPath, Addr: net.JoinHostPort("127.0.0.1", fmt.Sprint(apiPort)), Flow: "xtls-rprx-vision", Dir: dir, Timeout: 3 * time.Second}
	var users []Credential
	deadline := time.Now().Add(10 * time.Second)
	for {
		users, err = api.ListUsers(ctx)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("management API never came up: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("a config with no static client lists users %+v", users)
	}

	credential := Credential{ID: "11111111-2222-4333-8444-555555555555", Email: "cred-20260917T140000Z"}
	if err := api.AddUser(ctx, credential); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if err := api.AddUser(ctx, credential); err != nil {
		t.Fatalf("second AddUser must be idempotent: %v", err)
	}
	users, err = api.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].ID != credential.ID || users[0].Email != credential.Email {
		t.Fatalf("users after add = %+v", users)
	}
	if err := api.RemoveUser(ctx, credential.Email); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if err := api.RemoveUser(ctx, credential.Email); err != nil {
		t.Fatalf("second RemoveUser must be idempotent: %v", err)
	}
	users, err = api.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers after remove: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("users after remove = %+v", users)
	}
}

// requireXray locates the xray binary the integration tests drive. A
// developer machine without it skips; CI must have it (go-checks installs
// the release the relay image pins), so there a missing binary is a failure
// rather than a silently skipped security test.
func requireXray(t *testing.T) string {
	t.Helper()
	xrayPath, err := exec.LookPath("xray")
	if err == nil {
		return xrayPath
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("xray is not installed in CI; the relay integration tests must run there: %v", err)
	}
	t.Skip("xray is not installed")
	return ""
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
