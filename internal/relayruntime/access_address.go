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

// ipv4RouteProbeTarget is the route-lookup destination for IPv4RouteSource.
// Nothing is sent to it: connecting a UDP socket only resolves the route and
// the source address the kernel would use.
const ipv4RouteProbeTarget = "1.1.1.1:53"

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

// IPv4RouteSource returns the source address the kernel picks for outgoing
// IPv4 traffic to the internet, by connecting a UDP socket: a route lookup
// that sends nothing and needs no capability.
func IPv4RouteSource() (netip.Addr, error) {
	conn, err := net.Dial("udp4", ipv4RouteProbeTarget)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("IPv4 route lookup: %w", err)
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, errors.New("IPv4 route lookup returned no source address")
	}
	source, _ := netip.AddrFromSlice(local.IP)
	return source.Unmap(), nil
}

// VerifyIPv4AccessAddress reports whether addr is usable as the relay's IPv4
// access address: present on an interface and not the source the kernel uses
// for outgoing IPv4 (egressSource, from IPv4RouteSource). IPv4 has no
// deprecated state, so an IPv4 access address is a secondary address the
// routing table never selects, and the route lookup is what proves the
// relay's own traffic — registration, heartbeats, client egress — stays on
// the primary address.
func VerifyIPv4AccessAddress(addr netip.Addr, configured []netip.Addr, egressSource netip.Addr) error {
	present := false
	for _, candidate := range configured {
		if candidate == addr {
			present = true
			break
		}
	}
	switch {
	case !present:
		return fmt.Errorf("%s is not configured on any host interface", addr)
	case egressSource == addr:
		return fmt.Errorf("%s is the source address of outgoing IPv4 connections, which must stay on the primary address", addr)
	}
	return nil
}
