package volunteer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

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
				"tag":      "direct",
				"protocol": "freedom",
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
	cmd := exec.Command(xrayPath, "x25519")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return RealityKeyPair{}, fmt.Errorf("run %s x25519: %w: %s", xrayPath, err, strings.TrimSpace(string(out)))
	}
	return ParseRealityKeyPair(out)
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
