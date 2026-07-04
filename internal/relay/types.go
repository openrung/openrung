package relay

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ProtocolVLESSRealityVision = "vless-reality-vision"
	FlowVision                 = "xtls-rprx-vision"
	ExitModeDirect             = "direct"
	ExitModeDedicated          = "dedicated"

	// TransportDirect means clients reach the volunteer directly at its
	// advertised public endpoint. TransportTunnel means the endpoint is a relay
	// hub forwarding opaque bytes to a volunteer behind CGNAT over a reverse
	// tunnel.
	TransportDirect = "direct"
	TransportTunnel = "tunnel"
)

type RegisterRequest struct {
	PublicHost       string `json:"public_host"`
	PublicPort       int    `json:"public_port"`
	Protocol         string `json:"protocol"`
	ClientID         string `json:"client_id"`
	RealityPublicKey string `json:"reality_public_key"`
	ShortID          string `json:"short_id"`
	ServerName       string `json:"server_name"`
	Flow             string `json:"flow"`
	ExitMode         string `json:"exit_mode"`
	MaxSessions      int    `json:"max_sessions"`
	MaxMbps          int    `json:"max_mbps"`
	VolunteerVersion string `json:"volunteer_version"`
	Label            string `json:"label,omitempty"`
	Transport        string `json:"transport,omitempty"`
	// PunchCapable reports that this relay can attempt a direct NAT-hole-punched
	// path (client<->volunteer) via the hub's punch coordinator, bypassing the
	// hub data path. Only tunnel-transport volunteers set it. Clients that do not
	// understand it ignore it and use the advertised public endpoint as today.
	PunchCapable bool `json:"punch_capable,omitempty"`
	// PunchEndpoint is the hub's punch coordinator HTTP(S) base URL (e.g.
	// "https://203.0.113.1:9444"). The client uses it verbatim instead of
	// deriving one from PublicHost, so the scheme/host/port match the hub's
	// actual listener. Empty means "derive http://PublicHost:9444".
	PunchEndpoint string `json:"punch_endpoint,omitempty"`
}

type Descriptor struct {
	ID               string    `json:"id"`
	Label            string    `json:"label,omitempty"`
	PublicHost       string    `json:"public_host"`
	PublicPort       int       `json:"public_port"`
	Protocol         string    `json:"protocol"`
	ClientID         string    `json:"client_id"`
	RealityPublicKey string    `json:"reality_public_key"`
	ShortID          string    `json:"short_id"`
	ServerName       string    `json:"server_name"`
	Flow             string    `json:"flow"`
	ExitMode         string    `json:"exit_mode"`
	MaxSessions      int       `json:"max_sessions"`
	MaxMbps          int       `json:"max_mbps"`
	VolunteerVersion string    `json:"volunteer_version"`
	Transport        string    `json:"transport,omitempty"`
	PunchCapable     bool      `json:"punch_capable,omitempty"`
	PunchEndpoint    string    `json:"punch_endpoint,omitempty"`
	RegisteredAt     time.Time `json:"registered_at"`
	LastHeartbeatAt  time.Time `json:"last_heartbeat_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type ListResponse struct {
	Count      int          `json:"count"`
	ServerTime time.Time    `json:"server_time"`
	Relays     []Descriptor `json:"relays"`
}

type HeartbeatResponse struct {
	OK        bool      `json:"ok"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// MaxLabelLength bounds the operator-supplied relay label.
const MaxLabelLength = 63

// NormalizeLabel trims and validates an operator-supplied relay label. An empty
// label is allowed and returns "". Labels are restricted to a safe slug charset
// (letters, digits, hyphen, underscore, period) so they are safe to render in
// the admin dashboard and reusable as host/instance names.
func NormalizeLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", nil
	}
	if utf8.RuneCountInString(label) > MaxLabelLength {
		return "", fmt.Errorf("label must be at most %d characters", MaxLabelLength)
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "", errors.New("label may only contain letters, digits, hyphen (-), underscore (_), and period (.)")
		}
	}
	return label, nil
}
