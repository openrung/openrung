// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux

package relayruntime

// HostIPv6Addresses is unsupported off Linux: an access address is a server
// relay feature, and only Linux exposes the address flags it depends on.
func HostIPv6Addresses() ([]HostAddress, error) {
	return nil, ErrHostAddressesUnsupported
}
