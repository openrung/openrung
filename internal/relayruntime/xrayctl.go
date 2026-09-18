package relayruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// XrayUserManager adds and removes VLESS users on a running xray. The engine
// drives credential rotation through it; XrayAPI is the production
// implementation and tests substitute a recorder.
type XrayUserManager interface {
	AddUser(ctx context.Context, credential Credential) error
	RemoveUser(ctx context.Context, email string) error
}

// XrayAPI manages the users of the relay's VLESS inbound through the bundled
// xray binary's own `xray api` subcommands (HandlerService over the loopback
// management inbound BuildXrayConfig renders when APIPort is set; it is the
// only service exposed there). Shelling
// out to the binary the relay already ships keeps the relay free of xray's
// protobuf and gRPC dependencies, the same way Reality key generation shells
// out to `xray x25519`.
type XrayAPI struct {
	// Path locates the xray binary; empty means "xray" via PATH.
	Path string
	// Addr is the management inbound's host:port (loopback).
	Addr string
	// Flow is the VLESS flow every added user carries.
	Flow string
	// Dir receives the transient user fragments `xray api adu` reads (0600,
	// removed after each call). Empty means the OS temp dir.
	Dir string
	// Timeout bounds one API call; zero means xrayAPICallTimeout.
	Timeout time.Duration
}

const xrayAPICallTimeout = 5 * time.Second

// AddUser registers credential on the relay inbound. A credential that is
// already registered is not an error: every rotation step is idempotent so a
// retried step after a partial failure converges.
func (a *XrayAPI) AddUser(ctx context.Context, credential Credential) error {
	if credential.ID == "" || credential.Email == "" {
		return errors.New("xray api adu: credential id and email are required")
	}
	fragment, err := json.Marshal(map[string]any{
		"inbounds": []any{
			map[string]any{
				// adu builds the inbound from this fragment before adding its
				// users, so it needs a syntactically complete VLESS inbound:
				// a (never bound) port and the mandatory decryption field.
				"tag":      XrayInboundTag,
				"port":     1,
				"protocol": "vless",
				"settings": map[string]any{
					"clients": []any{
						map[string]any{
							"id":    credential.ID,
							"email": credential.Email,
							"flow":  a.Flow,
						},
					},
					"decryption": "none",
				},
			},
		},
	})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(a.Dir, "openrung-xray-user-*.json")
	if err != nil {
		return fmt.Errorf("xray api adu: create user fragment: %w", err)
	}
	fragmentPath := file.Name()
	defer os.Remove(fragmentPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("xray api adu: protect user fragment: %w", err)
	}
	if _, err := file.Write(fragment); err != nil {
		_ = file.Close()
		return fmt.Errorf("xray api adu: write user fragment: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("xray api adu: write user fragment: %w", err)
	}

	out, err := a.run(ctx, "adu", "-s", a.Addr, filepath.Clean(fragmentPath))
	if err != nil {
		return fmt.Errorf("xray api adu: %w", err)
	}
	// The subcommand exits 0 whether or not the user was added and reports the
	// outcome in its output, so the output is the result.
	switch {
	case strings.Contains(out, "Added 1 user(s)"), strings.Contains(out, "already exists"):
		return nil
	default:
		return fmt.Errorf("xray api adu did not add %s: %s", credential.Email, strings.TrimSpace(out))
	}
}

// RemoveUser drops the user labelled email from the relay inbound. Connections
// that user already authenticated stay up: VLESS checks the credential once,
// at the handshake. An unknown email is not an error, for the same
// idempotence reason as AddUser.
func (a *XrayAPI) RemoveUser(ctx context.Context, email string) error {
	if email == "" {
		return errors.New("xray api rmu: email is required")
	}
	out, err := a.run(ctx, "rmu", "-s", a.Addr, "-tag", XrayInboundTag, email)
	if err != nil {
		return fmt.Errorf("xray api rmu: %w", err)
	}
	switch {
	case strings.Contains(out, "Removed 1 user(s)"), strings.Contains(out, "not found"):
		return nil
	default:
		return fmt.Errorf("xray api rmu did not remove %s: %s", email, strings.TrimSpace(out))
	}
}

// ListUsers returns the users currently registered on the relay inbound.
func (a *XrayAPI) ListUsers(ctx context.Context) ([]Credential, error) {
	out, err := a.run(ctx, "inbounduser", "-s", a.Addr, "-tag", XrayInboundTag)
	if err != nil {
		return nil, fmt.Errorf("xray api inbounduser: %w", err)
	}
	var listed struct {
		Users []struct {
			Email   string `json:"email"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		} `json:"users"`
	}
	// The subcommand prints its JSON after any diagnostics; parse from the
	// first brace so a stray log line cannot break the decode.
	body := out
	if i := strings.Index(body, "{"); i >= 0 {
		body = body[i:]
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		return nil, fmt.Errorf("xray api inbounduser: parse %q: %w", strings.TrimSpace(out), err)
	}
	users := make([]Credential, 0, len(listed.Users))
	for _, u := range listed.Users {
		users = append(users, Credential{ID: u.Account.ID, Email: u.Email})
	}
	return users, nil
}

func (a *XrayAPI) run(ctx context.Context, args ...string) (string, error) {
	path := a.Path
	if path == "" {
		path = "xray"
	}
	timeout := a.Timeout
	if timeout == 0 {
		timeout = xrayAPICallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := NewXrayCommand(ctx, path, append([]string{"api"}, args...)...)
	ConfigureBackgroundCommand(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
