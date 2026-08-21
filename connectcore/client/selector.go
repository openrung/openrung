package client

import (
	"errors"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// Relay-selection sentinels. Kept in this package (not desktop) so
// clienttelemetry.ClassifyError can map them without importing a GUI package.
var (
	ErrNoUsableRelay     = errors.New("no usable relay")
	ErrNoRelaysAvailable = errors.New("broker returned no relays")
	ErrRelayNotInList    = errors.New("target relay not offered by the broker")
	ErrNoRelayInCountry  = errors.New("no usable relay in the requested country")
)

type RelayFamily string

const (
	RelayFamilyAuto RelayFamily = "auto"
	RelayFamilyIPv4 RelayFamily = "ipv4"
	RelayFamilyIPv6 RelayFamily = "ipv6"
)

func SelectRelay(resp brokerapi.RelayListResponse) (brokerapi.RelayDescriptor, error) {
	return SelectRelayForFamily(resp, RelayFamilyAuto)
}

func SelectRelayForFamily(resp brokerapi.RelayListResponse, family RelayFamily) (brokerapi.RelayDescriptor, error) {
	now := resp.ServerTime
	if now.IsZero() {
		now = time.Now()
	}

	for _, candidate := range resp.Relays {
		if !IsUsableRelay(candidate, now) {
			continue
		}
		isIPv6 := brokerapi.IsIPv6Host(candidate.PublicHost)
		switch family {
		case RelayFamilyIPv4:
			if !isIPv6 {
				return candidate, nil
			}
		case RelayFamilyIPv6:
			if isIPv6 {
				return candidate, nil
			}
		default:
			return candidate, nil
		}
	}
	return brokerapi.RelayDescriptor{}, ErrNoUsableRelay
}

func ParseRelayFamily(value string) (RelayFamily, error) {
	switch RelayFamily(value) {
	case "", RelayFamilyAuto:
		return RelayFamilyAuto, nil
	case RelayFamilyIPv4:
		return RelayFamilyIPv4, nil
	case RelayFamilyIPv6:
		return RelayFamilyIPv6, nil
	default:
		return "", errors.New("relay-family must be one of: auto, ipv4, ipv6")
	}
}

func IsUsableRelay(candidate brokerapi.RelayDescriptor, now time.Time) bool {
	return candidate.Protocol == brokerapi.ProtocolVLESSRealityVision &&
		candidate.Flow == brokerapi.FlowVision &&
		candidate.ExitMode == brokerapi.ExitModeDirect &&
		candidate.ExpiresAt.After(now) &&
		hasRequiredConnectionFields(candidate)
}

func hasRequiredConnectionFields(candidate brokerapi.RelayDescriptor) bool {
	return candidate.PublicHost != "" &&
		candidate.PublicPort > 0 &&
		candidate.ClientID != "" &&
		candidate.RealityPublicKey != "" &&
		candidate.ShortID != "" &&
		candidate.ServerName != ""
}
