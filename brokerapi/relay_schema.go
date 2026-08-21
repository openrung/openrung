// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// These private wire types validate the complete client-facing relay shape
// before a candidate can win FirstReachable. They intentionally omit the
// server-only relay fields whose internal Go model is not part of this module.
// Applications still receive and decode the exact verified JSON bytes.
type relayWireList struct {
	Count      *int                   `json:"count"`
	ServerTime *time.Time             `json:"server_time"`
	NotAfter   time.Time              `json:"not_after"`
	KeyID      string                 `json:"key_id"`
	Channel    string                 `json:"channel"`
	Limit      int                    `json:"limit,omitempty"`
	Relays     *[]relayWireDescriptor `json:"relays"`
}

type relayWireDescriptor struct {
	ID               *string         `json:"id"`
	Label            string          `json:"label,omitempty"`
	PublicHost       *string         `json:"public_host"`
	PublicPort       *int            `json:"public_port"`
	City             string          `json:"city,omitempty"`
	Country          string          `json:"country,omitempty"`
	CountryCode      string          `json:"country_code,omitempty"`
	Latitude         float64         `json:"latitude,omitempty"`
	Longitude        float64         `json:"longitude,omitempty"`
	NodeClass        string          `json:"node_class"`
	Protocol         *string         `json:"protocol"`
	ClientID         *string         `json:"client_id"`
	RealityPublicKey *string         `json:"reality_public_key"`
	ShortID          *string         `json:"short_id"`
	ServerName       *string         `json:"server_name"`
	Flow             *string         `json:"flow"`
	ExitMode         *string         `json:"exit_mode"`
	MaxSessions      *int            `json:"max_sessions"`
	MaxMbps          *int            `json:"max_mbps"`
	RelayVersion     string          `json:"relay_version"`
	VolunteerVersion *string         `json:"volunteer_version"`
	Transport        string          `json:"transport,omitempty"`
	PunchCapable     bool            `json:"punch_capable,omitempty"`
	PunchEndpoint    string          `json:"punch_endpoint,omitempty"`
	WSSFronts        []relayWSSFront `json:"wss_fronts,omitempty"`
	RegisteredAt     *time.Time      `json:"registered_at"`
	LastHeartbeatAt  *time.Time      `json:"last_heartbeat_at"`
	ExpiresAt        *time.Time      `json:"expires_at"`
}

type relayWSSFront struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocol_version"`
}

// UnmarshalJSON keeps the security-sensitive signed WSS-front shape exact.
// Top-level relay descriptors remain forward-compatible, but a nested front
// with missing or unknown fields must not be normalized before wsscore sees it.
func (f *relayWSSFront) UnmarshalJSON(data []byte) error {
	var decoded struct {
		ID              *string `json:"id"`
		URL             *string `json:"url"`
		ProtocolVersion *int    `json:"protocol_version"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if decoded.ID == nil || decoded.URL == nil || decoded.ProtocolVersion == nil {
		return fmt.Errorf("WSS front requires id, url, and protocol_version")
	}
	*f = relayWSSFront{
		ID:              *decoded.ID,
		URL:             *decoded.URL,
		ProtocolVersion: *decoded.ProtocolVersion,
	}
	return nil
}

// Exported client-facing relay schema (ADR-001 D3). These are the decoded
// model counterparts of the private wire-validation types above: clients (the
// connectcore module's engine and the mobile bindings) decode the exact
// verified JSON bytes a RelayList returns into these types instead of a
// root-internal model. The broker's server-side relay model — registration
// requests, identity proofs, and the never-serialized lease/exit fields —
// stays in the root module's internal/relay, which shares these wire
// constants; the cross-repo relay_decode contract vectors pin that both
// models keep decoding the same directory bytes the same way.

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

	// ChannelAPI, ChannelMirror, and ChannelInventory name the signed
	// relay-list channels. The value lives inside the signed body so a
	// long-lived mirror artifact can never be replayed into an API slot (or
	// vice versa): consumers check it against the channel they actually
	// fetched from.
	//
	// ChannelInventory is the credentialed operational snapshot served by
	// GET /admin/api/relays/inventory: the complete active relay set in stable
	// relay-ID order, never the client-facing candidate ranking. It is a
	// distinct channel precisely so an operator snapshot — untruncated, and so
	// a superset of any client page — can never be replayed into a client's
	// API or mirror slot, where the differing ordering and page contract would
	// otherwise go unnoticed.
	ChannelAPI       = "api"
	ChannelMirror    = "mirror"
	ChannelInventory = "inventory"

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

// RelayGeoLocation is the broker-resolved physical location of the relay's
// exit: exit_host for tunnel relays, public_host for direct relays. It is
// derived by the broker, never supplied by the relay, and is best-effort: all
// fields are empty when the lookup has not succeeded (yet).
type RelayGeoLocation struct {
	City        string `json:"city,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	// Latitude/Longitude let clients place the relay on a map. Zero values are
	// omitted, so "no coordinates" and "0,0" (open ocean) are indistinguishable
	// by design.
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
}

// RelayWSSFront advertises one CDN path to the WSS sidecar colocated with a
// relay. ID is bound into tickets and to that front's origin-token set; URL is
// public and contains no credential. The field set matches wsscore.Front —
// the type is declared here so this module's schema does not pull in the WSS
// transport module — and, like the rest of the decoded model, it decodes
// leniently: the strict all-fields-required check on nested fronts is the
// wire validation's job (relayWSSFront above), before any model sees a body.
type RelayWSSFront struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocol_version"`
}

// RelayDescriptor is one client-facing relay in a signed directory. It
// deliberately omits the broker's server-only fields (exit host, identity
// public key, lease token), which never appear in client-facing JSON.
type RelayDescriptor struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	PublicHost string `json:"public_host"`
	PublicPort int    `json:"public_port"`
	RelayGeoLocation
	// NodeClass is the broker-attested operator class (NodeClassFoundation or
	// NodeClassVolunteer). Always serialized, and covered by the relay-list
	// signature like every other descriptor field; clients that predate it
	// ignore it, clients that read it treat a missing value as the volunteer
	// class (see EffectiveNodeClass).
	NodeClass        string          `json:"node_class"`
	Protocol         string          `json:"protocol"`
	ClientID         string          `json:"client_id"`
	RealityPublicKey string          `json:"reality_public_key"`
	ShortID          string          `json:"short_id"`
	ServerName       string          `json:"server_name"`
	Flow             string          `json:"flow"`
	ExitMode         string          `json:"exit_mode"`
	MaxSessions      int             `json:"max_sessions"`
	MaxMbps          int             `json:"max_mbps"`
	RelayVersion     string          `json:"relay_version"`
	Transport        string          `json:"transport,omitempty"`
	PunchCapable     bool            `json:"punch_capable,omitempty"`
	PunchEndpoint    string          `json:"punch_endpoint,omitempty"`
	WSSFronts        []RelayWSSFront `json:"wss_fronts,omitempty"`
	RegisteredAt     time.Time       `json:"registered_at"`
	LastHeartbeatAt  time.Time       `json:"last_heartbeat_at"`
	ExpiresAt        time.Time       `json:"expires_at"`
}

// MarshalJSON emits the canonical key plus the deprecated v1 alias so bodies
// produced from this model (tests, fixtures) carry the same pair of version
// keys the broker serves.
func (d RelayDescriptor) MarshalJSON() ([]byte, error) {
	type descriptorAlias RelayDescriptor
	return json.Marshal(struct {
		descriptorAlias
		VolunteerVersion string `json:"volunteer_version"`
	}{
		descriptorAlias:  descriptorAlias(d),
		VolunteerVersion: d.RelayVersion,
	})
}

// UnmarshalJSON lets clients read both canonical broker responses and older
// v1 responses, which carried only the volunteer_version alias. When both
// keys are present, relay_version wins.
func (d *RelayDescriptor) UnmarshalJSON(data []byte) error {
	type descriptorAlias RelayDescriptor
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
	*d = RelayDescriptor(decoded)
	return nil
}

// RelayListResponse is the decoded signed relay directory. The whole
// marshaled body is covered by the detached Ed25519 signature in the
// X-OpenRung-Relays-Signature response header, so NotAfter/KeyID/Channel/Limit
// live here — carried in plain headers an attacker could rewrite them.
type RelayListResponse struct {
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
	// Channel is ChannelAPI, ChannelMirror, or ChannelInventory (see the
	// constants above).
	Channel string `json:"channel"`
	// Limit echoes the effective request limit on the API channel so clients
	// can reject a signed body replayed from a differently-shaped request.
	// Absent on the mirror channel, which is not request-shaped.
	Limit  int               `json:"limit,omitempty"`
	Relays []RelayDescriptor `json:"relays"`
}

// ErrorResponse is the broker's JSON error body shape.
type ErrorResponse struct {
	Error string `json:"error"`
}

// EffectiveNodeClass reads the class a client should act on from the value in
// a signed descriptor. Only the exact literal NodeClassFoundation grants the
// foundation class; an absent, unrecognized, or differently-cased value is the
// volunteer class.
//
// This is the read-side counterpart of the broker's ingest-side
// NormalizeNodeClass (root module internal/relay) and deliberately behaves
// differently. Ingest validates operator input, where an unrecognized class
// is an operator error worth rejecting, and where trimming and lowercasing
// are a convenience. Here the value has already been through that gate and is
// covered by the relay-list signature, so the only question left is what to
// do with a value this client does not know — and the answer has to be the
// less-privileged class, since the foundation class gates the WSS transport.
// Strictness is the point: a value that merely resembles "foundation" must
// not be read as it.
func EffectiveNodeClass(class string) string {
	if class == NodeClassFoundation {
		return NodeClassFoundation
	}
	return NodeClassVolunteer
}

// IsIPv6Host reports whether a descriptor's public host is an IPv6 literal
// (bracketed or bare). Clients use it for address-family relay selection and
// the broker for family-aware ranking.
func IsIPv6Host(host string) bool {
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

func validateRelayListWire(body []byte) error {
	var list relayWireList
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode relay list: %w", err)
	}
	if list.Count == nil || list.ServerTime == nil || list.Relays == nil {
		return fmt.Errorf("relay list requires count, server_time, and relays")
	}
	for index, relay := range *list.Relays {
		if relay.ID == nil ||
			relay.PublicHost == nil ||
			relay.PublicPort == nil ||
			relay.Protocol == nil ||
			relay.ClientID == nil ||
			relay.RealityPublicKey == nil ||
			relay.ShortID == nil ||
			relay.ServerName == nil ||
			relay.Flow == nil ||
			relay.ExitMode == nil ||
			relay.MaxSessions == nil ||
			relay.MaxMbps == nil ||
			relay.VolunteerVersion == nil ||
			relay.RegisteredAt == nil ||
			relay.LastHeartbeatAt == nil ||
			relay.ExpiresAt == nil {
			return fmt.Errorf("relay %d is missing a required client field", index)
		}
	}
	return nil
}
