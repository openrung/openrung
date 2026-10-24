// SPDX-License-Identifier: GPL-3.0-or-later

package relayruntime

import (
	"errors"
	"fmt"
	"net/netip"
)

// HostAddress is one IPv6 address configured on a host interface, with the
// kernel address flags the access-address check reads.
type HostAddress struct {
	IP netip.Addr
	// Deprecated is set for an address with a zero preferred lifetime. The
	// kernel still accepts connections to it and answers from it, but never
	// picks it as the source of a new outgoing connection.
	Deprecated bool
	// Tentative is set while duplicate address detection is still running;
	// the kernel drops traffic to a tentative address.
	Tentative bool
	// DADFailed is set when duplicate address detection found another holder.
	DADFailed bool
}

// ErrHostAddressesUnsupported reports a platform without the read-only
// address query VerifyAccessAddress needs (everything but Linux).
var ErrHostAddressesUnsupported = errors.New("listing host interface addresses with their flags is supported on Linux only")

// VerifyAccessAddress reports whether addr is usable as the relay's access
// address among the host's configured addresses: present on an interface,
// past duplicate address detection, and deprecated. Deprecation is what keeps
// the relay's own traffic — registration, heartbeats, client egress — on the
// primary address: the kernel never selects a deprecated address as a source,
// while inbound connections to it are served normally. The Foundation's
// fleet tooling configures it that way (preferred_lft 0).
func VerifyAccessAddress(addr netip.Addr, configured []HostAddress) error {
	for _, candidate := range configured {
		if candidate.IP != addr {
			continue
		}
		switch {
		case candidate.DADFailed:
			return fmt.Errorf("%s failed duplicate address detection", addr)
		case candidate.Tentative:
			return fmt.Errorf("%s is still tentative (duplicate address detection has not finished)", addr)
		case !candidate.Deprecated:
			return fmt.Errorf("%s is not deprecated (preferred_lft 0), so the kernel may pick it as the source of outgoing connections, which must stay on the primary address", addr)
		}
		return nil
	}
	return fmt.Errorf("%s is not configured on any host interface", addr)
}
