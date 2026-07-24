// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"encoding/json"
	"fmt"
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
