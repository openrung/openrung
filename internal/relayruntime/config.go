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

// blockOutboundTag routes a matched request into the blackhole outbound;
// directOutboundTag stays first in "outbounds" because Xray treats the first
// entry as the default for traffic no routing rule matches.
const (
	directOutboundTag = "direct"
	blockOutboundTag  = "block"
)

// privateDestinationCIDRs are the blocks a relay must never dial on a client's
// behalf. Without them the relay is an open door into wherever it happens to be
// hosted: loopback reaches services bound to the relay itself, RFC1918 reaches
// the provider's neighbouring machines, and link-local carries
// 169.254.169.254 -- the cloud metadata endpoint that hands out the instance's
// credentials to anyone who asks it from the box.
//
// Spelled out as literals rather than "geoip:private" on purpose: geoip.dat
// ships only in the relay container image, while desktop volunteer hosts
// resolve a bare xray binary that may have no assets beside it, and a geoip
// reference Xray cannot load makes it refuse to start. Literals need no assets.
var privateDestinationCIDRs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
}

type XrayConfigInput struct {
	ListenHost        string
	ListenPort        int
	ClientID          string
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
	if input.ClientID == "" {
		return nil, errors.New("client ID is required")
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

	cfg := map[string]any{
		"log": map[string]any{
			"loglevel": "warning",
		},
		"inbounds": []any{
			map[string]any{
				"tag":      "vless-reality-in",
				"listen":   input.ListenHost,
				"port":     input.ListenPort,
				"protocol": "vless",
				"settings": map[string]any{
					"clients": []any{
						map[string]any{
							"id":   input.ClientID,
							"flow": input.Flow,
						},
					},
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
				"tag":      directOutboundTag,
				"protocol": "freedom",
			},
			map[string]any{
				"tag":      blockOutboundTag,
				"protocol": "blackhole",
			},
		},
		// domainStrategy "IPIfNonMatch" resolves a domain destination and
		// re-checks the IP rules, so a hostname that points at private space
		// cannot walk past privateDestinationCIDRs. "AsIs" would skip that
		// second pass and leave the rule trivially bypassable. The cost is a
		// routing-layer DNS lookup (Xray-cached) per otherwise-unmatched
		// connection.
		"routing": map[string]any{
			"domainStrategy": "IPIfNonMatch",
			"rules": []any{
				// Sniffed BitTorrent is matched by handshake, not port: the
				// classic 6881-6889 range is vestigial on modern clients.
				// Depends on the inbound keeping "sniffing" enabled.
				map[string]any{
					"type":        "field",
					"protocol":    []string{"bittorrent"},
					"outboundTag": blockOutboundTag,
				},
				map[string]any{
					"type":        "field",
					"ip":          privateDestinationCIDRs,
					"outboundTag": blockOutboundTag,
				},
			},
		},
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
