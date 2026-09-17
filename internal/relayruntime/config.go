package relayruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	// IdentitySeedEnvironmentVariable carries the relay's long-lived Ed25519
	// identity seed. Xray never needs it, so every Xray subprocess is launched
	// without the relay's OPENRUNG_* environment namespace.
	IdentitySeedEnvironmentVariable = "OPENRUNG_IDENTITY_SEED"

	openRungEnvironmentVariablePrefix = "OPENRUNG_"

	// DefaultMaxSessions and DefaultMaxMbps are advertised capacity hints. The
	// relay does not enforce them directly; the broker uses them when ranking
	// candidates. Keep every relay surface on these shared defaults so the CLI
	// and embeddable volunteer engine cannot drift apart.
	DefaultMaxSessions = 75
	DefaultMaxMbps     = 100
)

// XrayInboundTag names the relay's public VLESS+Reality inbound. Runtime user
// management (XrayAPI) addresses that inbound by this tag, so it is fixed.
const XrayInboundTag = "vless-reality-in"

// xrayAPIInboundTag names the loopback dokodemo inbound that carries xray's
// own gRPC management API when XrayConfigInput.APIPort is set.
const xrayAPIInboundTag = "api-in"

type XrayConfigInput struct {
	ListenHost string
	ListenPort int
	// ClientID is the static VLESS credential baked into the config. It may
	// be empty only when APIPort is set: the relay then admits nobody until
	// it adds credentials at runtime through the management API.
	ClientID string
	// APIPort, when non-zero, enables xray's management API (HandlerService
	// for runtime user changes, StatsService for per-user counters) on a
	// loopback inbound at 127.0.0.1:APIPort. Zero renders today's static
	// single-client config with no API surface at all.
	APIPort           int
	Flow              string
	Dest              string
	ServerName        string
	RealityPrivateKey string
	ShortID           string
}

type RealityKeyPair struct {
	PrivateKey string
	PublicKey  string
}

func BuildXrayConfig(input XrayConfigInput) ([]byte, error) {
	if input.ListenHost == "" {
		input.ListenHost = "::"
	}
	if input.ListenPort < 1 || input.ListenPort > 65535 {
		return nil, errors.New("listen port must be between 1 and 65535")
	}
	if input.ClientID == "" && input.APIPort == 0 {
		return nil, errors.New("client ID is required")
	}
	if input.APIPort < 0 || input.APIPort > 65535 {
		return nil, errors.New("api port must be between 0 and 65535")
	}
	if input.APIPort != 0 && input.APIPort == input.ListenPort {
		return nil, errors.New("api port must differ from the listen port")
	}
	if input.Flow == "" {
		return nil, errors.New("flow is required")
	}
	if input.Dest == "" {
		return nil, errors.New("reality dest is required")
	}
	if input.ServerName == "" {
		return nil, errors.New("server name is required")
	}
	if input.RealityPrivateKey == "" {
		return nil, errors.New("reality private key is required")
	}
	if input.ShortID == "" {
		return nil, errors.New("short ID is required")
	}

	// An empty list (not null) keeps xray's VLESS validator well-formed while
	// every credential arrives through the management API.
	clients := []any{}
	if input.ClientID != "" {
		clients = append(clients, map[string]any{
			"id":   input.ClientID,
			"flow": input.Flow,
		})
	}
	cfg := map[string]any{
		"log": map[string]any{
			"loglevel": "warning",
		},
		"inbounds": []any{
			map[string]any{
				"tag":      XrayInboundTag,
				"listen":   input.ListenHost,
				"port":     input.ListenPort,
				"protocol": "vless",
				"settings": map[string]any{
					"clients":    clients,
					"decryption": "none",
				},
				"streamSettings": map[string]any{
					"network":  "tcp",
					"security": "reality",
					"realitySettings": map[string]any{
						"show":        false,
						"dest":        input.Dest,
						"xver":        0,
						"serverNames": []string{input.ServerName},
						"privateKey":  input.RealityPrivateKey,
						"shortIds":    []string{input.ShortID},
					},
				},
				"sniffing": map[string]any{
					"enabled":      true,
					"destOverride": []string{"http", "tls", "quic"},
				},
			},
		},
		"outbounds": []any{
			map[string]any{
				"tag":      "direct",
				"protocol": "freedom",
			},
		},
	}
	if input.APIPort != 0 {
		// The standard xray management layout: the "api" object registers the
		// gRPC services and implicitly creates an outbound handler tagged
		// "api"; a loopback dokodemo inbound accepts the client connections and
		// a routing rule steers exactly that inbound to it. User-level stats
		// need the policy switches or the per-user counters never appear.
		cfg["api"] = map[string]any{
			"tag":      "api",
			"services": []string{"HandlerService", "StatsService"},
		}
		cfg["stats"] = map[string]any{}
		cfg["policy"] = map[string]any{
			"levels": map[string]any{
				"0": map[string]any{
					"statsUserUplink":   true,
					"statsUserDownlink": true,
				},
			},
		}
		cfg["inbounds"] = append(cfg["inbounds"].([]any), map[string]any{
			"tag":      xrayAPIInboundTag,
			"listen":   "127.0.0.1",
			"port":     input.APIPort,
			"protocol": "dokodemo-door",
			"settings": map[string]any{
				"address": "127.0.0.1",
			},
		})
		cfg["routing"] = map[string]any{
			"rules": []any{
				map[string]any{
					"type":        "field",
					"inboundTag":  []string{xrayAPIInboundTag},
					"outboundTag": "api",
				},
			},
		}
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func GenerateShortID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func GenerateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func GenerateRealityKeyPair(xrayPath string) (RealityKeyPair, error) {
	if xrayPath == "" {
		xrayPath = "xray"
	}
	cmd := NewXrayCommand(context.Background(), xrayPath, "x25519")
	ConfigureBackgroundCommand(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return RealityKeyPair{}, fmt.Errorf("run %s x25519: %w: %s", xrayPath, err, strings.TrimSpace(string(out)))
	}
	return ParseRealityKeyPair(out)
}

// NewXrayCommand constructs an Xray subprocess without passing relay-owned
// environment variables to it. This shared boundary covers both one-shot key
// generation and the long-running relay process.
func NewXrayCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = environmentForXray(os.Environ())
	return cmd
}

func environmentForXray(environ []string) []string {
	// Keep the ordinary process environment because Xray may need PATH, proxy or
	// certificate settings, and XRAY_* runtime configuration. Xray receives all
	// relay configuration through its config file and never needs OPENRUNG_*.
	// Filtering the whole application namespace is safer than maintaining a list
	// of today's secrets: future relay credentials are excluded by default too.
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if !hasCaseInsensitivePrefix(name, openRungEnvironmentVariablePrefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func hasCaseInsensitivePrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func ParseRealityKeyPair(out []byte) (RealityKeyPair, error) {
	privateRe := regexp.MustCompile(`(?im)^\s*Private\s*Key:\s*([A-Za-z0-9_-]+)\s*$`)
	publicRe := regexp.MustCompile(`(?im)^\s*(?:Public\s*Key|Password\s*\(PublicKey\)):\s*([A-Za-z0-9_-]+)\s*$`)

	privateMatch := privateRe.FindSubmatch(out)
	publicMatch := publicRe.FindSubmatch(out)
	if privateMatch == nil || publicMatch == nil {
		return RealityKeyPair{}, fmt.Errorf("could not parse xray x25519 output: %s", strings.TrimSpace(string(out)))
	}

	return RealityKeyPair{
		PrivateKey: string(bytes.TrimSpace(privateMatch[1])),
		PublicKey:  string(bytes.TrimSpace(publicMatch[1])),
	}, nil
}
