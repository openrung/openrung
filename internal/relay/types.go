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

	// TransportDirect means clients reach the relay directly at its
	// advertised public endpoint. TransportTunnel means the endpoint is a relay
	// hub forwarding opaque bytes to a relay behind CGNAT over a reverse
	// tunnel.
	TransportDirect = "direct"
	TransportTunnel = "tunnel"

	// ChannelAPI and ChannelMirror name the two signed relay-list channels.
	// The value lives inside the signed body so a long-lived mirror artifact
	// can never be replayed into an API slot (or vice versa): clients check it
	// against the channel they actually fetched from.
	ChannelAPI    = "api"
	ChannelMirror = "mirror"

	// NodeClassFoundation marks a relay operated by the OpenRung Foundation
	// itself; NodeClassVolunteer (the default) marks community-operated
	// hardware. The class records provenance — who runs the relay — not a
	// quality score: reliability is measured per-relay by telemetry either
	// way. The broker only accepts a foundation claim from a registration
	// that presents the foundation token, and the class travels inside the
	// signed relay-list body, so clients can trust it without any new
	// verification machinery.
	NodeClassFoundation = "foundation"
	NodeClassVolunteer  = "volunteer"
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
	// RelayVersion retains the legacy volunteer_version JSON name for wire
	// compatibility with deployed brokers, relays, and clients.
	RelayVersion string `json:"volunteer_version"`
	Label        string `json:"label,omitempty"`
	// NodeClass declares who operates this relay: NodeClassVolunteer (the
	// default when empty) or NodeClassFoundation. A foundation claim is only
	// honored when the request presents the broker's foundation token;
	// otherwise registration is rejected, so the class can never be
	// self-granted.
	NodeClass string `json:"node_class,omitempty"`
	Transport string `json:"transport,omitempty"`
	// PunchCapable reports that this relay can attempt a direct NAT-hole-punched
	// path (client<->relay) via the hub's punch coordinator, bypassing the
	// hub data path. Only tunnel-transport relays set it. Clients that do not
	// understand it ignore it and use the advertised public endpoint as today.
	PunchCapable bool `json:"punch_capable,omitempty"`
	// PunchEndpoint is the hub's punch coordinator HTTP(S) base URL (e.g.
	// "https://203.0.113.1:9444"). The client uses it verbatim instead of
	// deriving one from PublicHost, so the scheme/host/port match the hub's
	// actual listener. Empty means "derive http://PublicHost:9444".
	PunchEndpoint string `json:"punch_endpoint,omitempty"`
	// ExitHost is set by the relay hub for tunnel-transport registrations: the
	// relay's source IP as observed on its control connection, i.e. where
	// tunneled traffic actually exits. The broker uses it only to geolocate the
	// relay (instead of PublicHost, which is the hub for tunnel transport) and
	// never serves it to clients. Rejected for direct transport, where
	// PublicHost already is the exit.
	ExitHost string `json:"exit_host,omitempty"`
}

// GeoLocation is the broker-resolved physical location of the relay's exit:
// exit_host for tunnel relays, public_host for direct relays. It is derived by
// the broker, never supplied by the relay, and is best-effort: all fields
// are empty when the lookup has not succeeded (yet).
type GeoLocation struct {
	City        string `json:"city,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	// Latitude/Longitude let clients place the relay on a map. Zero values are
	// omitted, so "no coordinates" and "0,0" (open ocean) are indistinguishable
	// by design.
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
}

type Descriptor struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	PublicHost string `json:"public_host"`
	PublicPort int    `json:"public_port"`
	GeoLocation
	// ExitHost is stored so heartbeat-time geo backfills keep resolving the
	// true exit location of tunnel relays, but it is never serialized: exposing
	// a CGNAT relay's observed exit IP through the public API would defeat the
	// privacy the hub provides.
	ExitHost string `json:"-"`
	// NodeClass is the broker-attested operator class (NodeClassFoundation or
	// NodeClassVolunteer). Always serialized, and covered by the relay-list
	// signature like every other descriptor field; clients that predate it
	// ignore it, clients that read it treat a missing value as the volunteer class.
	NodeClass        string `json:"node_class"`
	Protocol         string `json:"protocol"`
	ClientID         string `json:"client_id"`
	RealityPublicKey string `json:"reality_public_key"`
	ShortID          string `json:"short_id"`
	ServerName       string `json:"server_name"`
	Flow             string `json:"flow"`
	ExitMode         string `json:"exit_mode"`
	MaxSessions      int    `json:"max_sessions"`
	MaxMbps          int    `json:"max_mbps"`
	// RelayVersion retains the legacy volunteer_version JSON name for wire
	// compatibility with deployed brokers, relays, and clients.
	RelayVersion    string    `json:"volunteer_version"`
	Transport       string    `json:"transport,omitempty"`
	PunchCapable    bool      `json:"punch_capable,omitempty"`
	PunchEndpoint   string    `json:"punch_endpoint,omitempty"`
	RegisteredAt    time.Time `json:"registered_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// ListResponse is the signed relay directory. The whole marshaled body is
// covered by the detached Ed25519 signature in the X-OpenRung-Relays-Signature
// response header, so NotAfter/KeyID/Channel/Limit must live here — carried in
// plain headers an attacker could rewrite them.
type ListResponse struct {
	Count      int       `json:"count"`
	ServerTime time.Time `json:"server_time"`
	// NotAfter bounds replay of a validly signed body: ServerTime + 30 min on
	// the API channel, publish time + 24 h on the mirror channel. Clients
	// reject responses past it (with a small clock-skew allowance).
	NotAfter time.Time `json:"not_after"`
	// KeyID is lowercase hex of the first 8 bytes of SHA-256 over the raw
	// 32-byte Ed25519 signing public key. Advisory routing only: clients fall
	// back to trying every pinned key when it matches none of them.
	KeyID string `json:"key_id"`
	// Channel is ChannelAPI or ChannelMirror (see the constants above).
	Channel string `json:"channel"`
	// Limit echoes the effective request limit on the API channel so clients
	// can reject a signed body replayed from a differently-shaped request.
	// Absent on the mirror channel, which is not request-shaped.
	Limit  int          `json:"limit,omitempty"`
	Relays []Descriptor `json:"relays"`
}

type HeartbeatResponse struct {
	OK        bool      `json:"ok"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// NormalizeNodeClass trims, lowercases, and validates an operator-supplied
// node class. Empty means "unstated" and normalizes to NodeClassVolunteer, so
// every descriptor downstream carries a concrete class.
func NormalizeNodeClass(class string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", NodeClassVolunteer:
		return NodeClassVolunteer, nil
	case NodeClassFoundation:
		return NodeClassFoundation, nil
	default:
		return "", fmt.Errorf("node_class must be %q or %q", NodeClassVolunteer, NodeClassFoundation)
	}
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
