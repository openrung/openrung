// SPDX-License-Identifier: GPL-3.0-or-later

package relayruntime

import (
	"errors"
	"fmt"
	"net"
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
// address and route queries the access-address checks need (everything but
// Linux).
var ErrHostAddressesUnsupported = errors.New("listing host interface addresses with their flags, and routing tables, is supported on Linux only")

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

// IPv4Route is one IPv4 unicast route from any of the host's routing tables,
// with the fields the IPv4 access-address check reads.
type IPv4Route struct {
	Table uint32
	Dst   netip.Prefix
	// PrefSrc is the route's preferred source address ("src"), or the zero
	// Addr when the route names none and the kernel picks one per lookup.
	PrefSrc netip.Addr
}

// HostIPv4Addresses lists the IPv4 addresses configured on the host's
// interfaces.
func HostIPv4Addresses() ([]netip.Addr, error) {
	interfaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list interface addresses: %w", err)
	}
	var addresses []netip.Addr
	for _, interfaceAddr := range interfaceAddrs {
		if ip, ok := netip.AddrFromSlice(addressIP(interfaceAddr)); ok && ip.Unmap().Is4() {
			addresses = append(addresses, ip.Unmap())
		}
	}
	return addresses, nil
}

// VerifyIPv4AccessAddress reports whether addr is usable as the relay's IPv4
// access address: present on an interface, and never the source of outgoing
// IPv4. IPv4 has no deprecated state, so the guarantee comes from the routing
// tables instead: an unbound socket's source is the matching route's
// preferred source when it names one, and otherwise an address the kernel
// picks from the interface, which can be the access address. So every
// unicast route in every table must name a preferred source other than addr.
// Local and broadcast routes carry no outgoing internet traffic and are not
// unicast routes, so they are not listed.
func VerifyIPv4AccessAddress(addr netip.Addr, configured []netip.Addr, routes []IPv4Route) error {
	present := false
	for _, candidate := range configured {
		if candidate == addr {
			present = true
			break
		}
	}
	if !present {
		return fmt.Errorf("%s is not configured on any host interface", addr)
	}
	for _, route := range routes {
		switch {
		case !route.PrefSrc.IsValid():
			return fmt.Errorf("the route to %s (table %d) names no preferred source (src), so the kernel may pick %s as the source of outgoing IPv4, which must stay on the primary address", route.Dst, route.Table, addr)
		case route.PrefSrc == addr:
			return fmt.Errorf("the route to %s (table %d) uses %s as its source, which must stay on the primary address", route.Dst, route.Table, addr)
		}
	}
	return nil
}
