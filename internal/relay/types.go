package relay

import (
	"encoding/json"
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

	// WSSProtocolVersion is the opaque Reality-over-WebSocket protocol spoken
	// between a desktop client's loopback adapter and a relay-local sidecar.
	// Unknown versions are ignored so direct Reality remains backward-compatible.
	WSSProtocolVersion = 1

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
	RelayVersion     string `json:"relay_version"`
	Label            string `json:"label,omitempty"`
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
	// WSSFronts are CDN URLs that terminate at this exact relay's local WSS
	// sidecar. The broker accepts them only from a direct-mode Foundation relay
	// on public port 443 with a stable identity, then includes them in the
	// signed directory. They never describe a shared router or another relay.
	WSSFronts []WSSFrontDescriptor `json:"wss_fronts,omitempty"`
	// WSSCapabilityProof is a separate identity-key signature over this
	// registration plus the ordered WSSFronts list. It deliberately does not
	// alter the deployed relay-identity-v1 statement. All capability fields must
	// travel together and are private registration material, not directory data.
	WSSCapabilityProof     string `json:"wss_capability_proof,omitempty"`
	WSSCapabilityExpiresAt string `json:"wss_capability_expires_at,omitempty"`
	// ExitHost is set by the relay hub for tunnel-transport registrations: the
	// relay's source IP as observed on its control connection, i.e. where
	// tunneled traffic actually exits. The broker uses it only to geolocate the
	// relay (instead of PublicHost, which is the hub for tunnel transport) and
	// never serves it to clients. Rejected for direct transport, where
	// PublicHost already is the exit.
	ExitHost string `json:"exit_host,omitempty"`
	// IdentityPublicKey/IdentityProof/IdentityExpiresAt carry the optional
	// stable-identity proof (spec openrung-relay-identity-v1, see identity.go).
	// All three travel together; a registration without them keeps the legacy
	// random relay ID. Old brokers ignore these unknown fields, so a new relay
	// registers fine (with a random ID) during a rolling deploy.
	IdentityPublicKey string `json:"identity_public_key,omitempty"`
	IdentityProof     string `json:"identity_proof,omitempty"`
	IdentityExpiresAt string `json:"identity_expires_at,omitempty"`
}

// MarshalJSON emits the canonical key plus the deprecated v1 alias so upgraded
// relays and hubs can still register their version with an older broker during
// a rolling deployment.
func (r RegisterRequest) MarshalJSON() ([]byte, error) {
	type registerRequestAlias RegisterRequest
	return json.Marshal(struct {
		registerRequestAlias
		VolunteerVersion string `json:"volunteer_version"`
	}{
		registerRequestAlias: registerRequestAlias(r),
		VolunteerVersion:     r.RelayVersion,
	})
}

// UnmarshalJSON accepts the pre-relay-terminology volunteer_version key so
// deployed relays can continue registering during the v1 migration. When both
// keys are present, the canonical relay_version value wins.
func (r *RegisterRequest) UnmarshalJSON(data []byte) error {
	type registerRequestAlias RegisterRequest
	var decoded registerRequestAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var versions struct {
		RelayVersion     *string `json:"relay_version"`
		VolunteerVersion *string `json:"volunteer_version"`
	}
	if err := json.Unmarshal(data, &versions); err != nil {
		return err
	}
	switch {
	case versions.RelayVersion != nil:
		decoded.RelayVersion = *versions.RelayVersion
	case versions.VolunteerVersion != nil:
		decoded.RelayVersion = *versions.VolunteerVersion
	}
	*r = RegisterRequest(decoded)
	return nil
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

// WSSFrontDescriptor advertises one CDN path to the sidecar colocated with a
// relay. ID is bound into tickets and to that front's origin-token set; URL is
// public and contains no credential. Multiple entries let one relay use
// independent CDN distributions without creating a shared data-plane service.
type WSSFrontDescriptor struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocol_version"`
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
	// IdentityPublicKey is the base64 Ed25519 key a stable-identity relay
	// proved possession of at registration (empty for legacy registrations).
	// Stored for operations and future per-relay auth; not serialized — the
	// relay ID it derives is the public handle, and the list stays lean.
	IdentityPublicKey string `json:"-"`
	// LeaseToken identifies one concrete registration of this relay ID. Stable
	// identities deliberately reuse their public ID across reconnects, so the
	// ID alone cannot authorize a heartbeat: an older session would otherwise
	// keep a newer session's endpoint alive. The token is returned only by the
	// registration endpoint and is never included in public relay lists.
	LeaseToken string `json:"-"`
	// NodeClass is the broker-attested operator class (NodeClassFoundation or
	// NodeClassVolunteer). Always serialized, and covered by the relay-list
	// signature like every other descriptor field; clients that predate it
	// ignore it, clients that read it treat a missing value as the volunteer class.
	NodeClass        string               `json:"node_class"`
	Protocol         string               `json:"protocol"`
	ClientID         string               `json:"client_id"`
	RealityPublicKey string               `json:"reality_public_key"`
	ShortID          string               `json:"short_id"`
	ServerName       string               `json:"server_name"`
	Flow             string               `json:"flow"`
	ExitMode         string               `json:"exit_mode"`
	MaxSessions      int                  `json:"max_sessions"`
	MaxMbps          int                  `json:"max_mbps"`
	RelayVersion     string               `json:"relay_version"`
	Transport        string               `json:"transport,omitempty"`
	PunchCapable     bool                 `json:"punch_capable,omitempty"`
	PunchEndpoint    string               `json:"punch_endpoint,omitempty"`
	WSSFronts        []WSSFrontDescriptor `json:"wss_fronts,omitempty"`
	RegisteredAt     time.Time            `json:"registered_at"`
	LastHeartbeatAt  time.Time            `json:"last_heartbeat_at"`
	ExpiresAt        time.Time            `json:"expires_at"`
}

// RegisterResponse is the private response to a successful registration. It
// has the same wire shape as Descriptor plus lease_token; keeping the token out
// of Descriptor's JSON representation prevents it from leaking through signed
// public relay-list responses.
type RegisterResponse struct {
	Descriptor Descriptor
}

func (r RegisterResponse) MarshalJSON() ([]byte, error) {
	type descriptorAlias Descriptor
	return json.Marshal(struct {
		descriptorAlias
		VolunteerVersion string `json:"volunteer_version"`
		LeaseToken       string `json:"lease_token,omitempty"`
	}{
		descriptorAlias:  descriptorAlias(r.Descriptor),
		VolunteerVersion: r.Descriptor.RelayVersion,
		LeaseToken:       r.Descriptor.LeaseToken,
	})
}

func (r *RegisterResponse) UnmarshalJSON(data []byte) error {
	var desc Descriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return err
	}
	var private struct {
		LeaseToken string `json:"lease_token"`
	}
	if err := json.Unmarshal(data, &private); err != nil {
		return err
	}
	desc.LeaseToken = private.LeaseToken
	r.Descriptor = desc
	return nil
}

// HeartbeatRequest renews exactly the registration that received LeaseToken.
// Old identityless relays may omit it during a rolling upgrade; stable-
// identity registrations require it because their relay ID is reusable.
type HeartbeatRequest struct {
	OK         bool   `json:"ok,omitempty"`
	LeaseToken string `json:"lease_token,omitempty"`
}

// MarshalJSON keeps volunteer_version as a deprecated v1 response alias while
// released native clients still require it. New clients should read
// relay_version; both keys carry the same value and are covered by the relay-list
// signature.
func (d Descriptor) MarshalJSON() ([]byte, error) {
	type descriptorAlias Descriptor
	return json.Marshal(struct {
		descriptorAlias
		VolunteerVersion string `json:"volunteer_version"`
	}{
		descriptorAlias:  descriptorAlias(d),
		VolunteerVersion: d.RelayVersion,
	})
}

// UnmarshalJSON lets current clients read both canonical broker responses and
// older v1 responses. When both keys are present, relay_version wins.
func (d *Descriptor) UnmarshalJSON(data []byte) error {
	type descriptorAlias Descriptor
	var decoded descriptorAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var versions struct {
		RelayVersion     *string `json:"relay_version"`
		VolunteerVersion *string `json:"volunteer_version"`
	}
	if err := json.Unmarshal(data, &versions); err != nil {
		return err
	}
	switch {
	case versions.RelayVersion != nil:
		decoded.RelayVersion = *versions.RelayVersion
	case versions.VolunteerVersion != nil:
		decoded.RelayVersion = *versions.VolunteerVersion
	}
	*d = Descriptor(decoded)
	return nil
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

// WSSSessionTicketRequest asks for one short-lived, single-use ticket bound to
// an exact relay and one of that relay's currently advertised CDN fronts.
type WSSSessionTicketRequest struct {
	RelayID string `json:"relay_id"`
	FrontID string `json:"front_id"`
}

// WSSSessionTicketResponse returns the opaque ticket with the exact URL chosen
// from the same live descriptor. Clients reject a response URL that differs
// from the signed directory entry and send Ticket only as an Authorization
// bearer during the WebSocket upgrade.
type WSSSessionTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
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
