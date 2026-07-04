package client

import (
	"errors"
	"time"

	"openrung/internal/relay"
)

var ErrNoUsableRelay = errors.New("no usable relay")

type RelayFamily string

const (
	RelayFamilyAuto RelayFamily = "auto"
	RelayFamilyIPv4 RelayFamily = "ipv4"
	RelayFamilyIPv6 RelayFamily = "ipv6"
)

func SelectRelay(resp relay.ListResponse) (relay.Descriptor, error) {
	return SelectRelayForFamily(resp, RelayFamilyAuto)
}

func SelectRelayForFamily(resp relay.ListResponse, family RelayFamily) (relay.Descriptor, error) {
	now := resp.ServerTime
	if now.IsZero() {
		now = time.Now()
	}

	for _, candidate := range resp.Relays {
		if !IsUsableRelay(candidate, now) {
			continue
		}
		isIPv6 := relay.IsIPv6Host(candidate.PublicHost)
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
	return relay.Descriptor{}, ErrNoUsableRelay
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

func IsUsableRelay(candidate relay.Descriptor, now time.Time) bool {
	return candidate.Protocol == relay.ProtocolVLESSRealityVision &&
		candidate.Flow == relay.FlowVision &&
		candidate.ExitMode == relay.ExitModeDirect &&
		candidate.ExpiresAt.After(now) &&
		hasRequiredConnectionFields(candidate)
}

func hasRequiredConnectionFields(candidate relay.Descriptor) bool {
	return candidate.PublicHost != "" &&
		candidate.PublicPort > 0 &&
		candidate.ClientID != "" &&
		candidate.RealityPublicKey != "" &&
		candidate.ShortID != "" &&
		candidate.ServerName != ""
}
