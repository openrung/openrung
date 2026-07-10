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

	// ChannelAPI and ChannelMirror label the two signed relay-list channels
	// (relay-list signing SPEC v1 §2.2): broker fronts and the direct origin
	// serve "api", static mirror artifacts serve "mirror". The channel lives
	// inside the signed body so a verifier can reject a validly signed body
	// replayed across channels (a mirror artifact fed into an API slot would
	// otherwise pass with a 24 h freshness window instead of 30 min).
	ChannelAPI    = "api"
	ChannelMirror = "mirror"
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
	// ExitHost is set by the relay hub for tunnel-transport registrations: the
	// volunteer's source IP as observed on its control connection, i.e. where
	// tunneled traffic actually exits. The broker uses it only to geolocate the
	// relay (instead of PublicHost, which is the hub for tunnel transport) and
	// never serves it to clients. Rejected for direct transport, where
	// PublicHost already is the exit.
	ExitHost string `json:"exit_host,omitempty"`
}

// GeoLocation is the broker-resolved physical location of the relay's exit:
// exit_host for tunnel relays, public_host for direct relays. It is derived by
// the broker, never supplied by the volunteer, and is best-effort: all fields
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
	// a CGNAT volunteer's real IP through the public API would defeat the
	// privacy the hub provides.
	ExitHost         string    `json:"-"`
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
	Count      int       `json:"count"`
	ServerTime time.Time `json:"server_time"`
	// Signing fields (relay-list signing SPEC v1 §2.2), added by the broker to
	// every signed 2xx list response. Pre-signing brokers omit them, and the Go
	// zero values fail every client verification check closed. NotAfter bounds
	// how long a signed body may be replayed (server_time + 30 min on the API
	// channel, publish time + 24 h on the mirror channel). KeyID is lowercase
	// hex of the first 8 bytes of SHA-256 over the raw signing public key —
	// advisory routing into the client's pinned key set, never trusted. Channel
	// binds the body to the channel it was fetched from (ChannelAPI or
	// ChannelMirror). Limit echoes the effective request limit on the API
	// channel and is absent on the mirror channel; clients reject a mismatch so
	// a validly signed limit=1 body cannot be steered into a limit=20 request.
	// Field order matters: the broker signs json.Marshal output, and the shared
	// test vector pins count, server_time, not_after, key_id, channel, limit,
	// relays.
	NotAfter time.Time    `json:"not_after,omitzero"`
	KeyID    string       `json:"key_id,omitempty"`
	Channel  string       `json:"channel,omitempty"`
	Limit    int          `json:"limit,omitempty"`
	Relays   []Descriptor `json:"relays"`
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
