// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package relayruntime

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// ifaFlags is IFA_FLAGS, the 32-bit address flags attribute that supersedes
// ifa_flags' eight bits on kernels since 3.14; the syscall package lacks it.
const ifaFlags = 8

// HostIPv6Addresses lists the IPv6 addresses configured on the host's
// interfaces with one RTM_GETADDR netlink dump. The dump is read-only and
// needs no capability, so it works from the relay container, which shares
// the host network namespace with every capability dropped.
func HostIPv6Addresses() ([]HostAddress, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_INET6)
	if err != nil {
		return nil, fmt.Errorf("netlink address dump: %w", err)
	}
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil, fmt.Errorf("parse netlink address dump: %w", err)
	}
	return parseIPv6AddressMessages(messages)
}

func parseIPv6AddressMessages(messages []syscall.NetlinkMessage) ([]HostAddress, error) {
	var addresses []HostAddress
	for i := range messages {
		message := &messages[i]
		if message.Header.Type == syscall.NLMSG_DONE {
			break
		}
		if message.Header.Type != syscall.RTM_NEWADDR || len(message.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		// struct ifaddrmsg: family, prefixlen, flags, scope (one byte each),
		// then the interface index.
		if message.Data[0] != syscall.AF_INET6 {
			continue
		}
		flags := uint32(message.Data[2])
		attributes, err := syscall.ParseNetlinkRouteAttr(message)
		if err != nil {
			return nil, fmt.Errorf("parse netlink address attributes: %w", err)
		}
		var ip netip.Addr
		for _, attribute := range attributes {
			switch attribute.Attr.Type {
			case syscall.IFA_ADDRESS:
				// For IPv6, IFA_ADDRESS is the local address (there is no
				// separate IFA_LOCAL as for point-to-point IPv4).
				if parsed, ok := netip.AddrFromSlice(attribute.Value); ok {
					ip = parsed
				}
			case ifaFlags:
				if len(attribute.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(attribute.Value)
				}
			}
		}
		if !ip.Is6() {
			continue
		}
		addresses = append(addresses, HostAddress{
			IP:         ip,
			Deprecated: flags&syscall.IFA_F_DEPRECATED != 0,
			Tentative:  flags&syscall.IFA_F_TENTATIVE != 0,
			DADFailed:  flags&syscall.IFA_F_DADFAILED != 0,
		})
	}
	return addresses, nil
}

// HostIPv4UnicastRoutes lists the unicast routes of every IPv4 routing table
// with one RTM_GETROUTE netlink dump, which is read-only and unprivileged
// like the address dump.
func HostIPv4UnicastRoutes() ([]IPv4Route, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("netlink route dump: %w", err)
	}
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil, fmt.Errorf("parse netlink route dump: %w", err)
	}
	return parseIPv4RouteMessages(messages)
}

func parseIPv4RouteMessages(messages []syscall.NetlinkMessage) ([]IPv4Route, error) {
	var routes []IPv4Route
	for i := range messages {
		message := &messages[i]
		if message.Header.Type == syscall.NLMSG_DONE {
			break
		}
		if message.Header.Type != syscall.RTM_NEWROUTE || len(message.Data) < syscall.SizeofRtMsg {
			continue
		}
		// struct rtmsg: family, dst_len, src_len, tos, table, protocol,
		// scope, type (one byte each), then the flags.
		if message.Data[0] != syscall.AF_INET || message.Data[7] != syscall.RTN_UNICAST {
			continue
		}
		dstLen := int(message.Data[1])
		route := IPv4Route{Table: uint32(message.Data[4])}
		dst := netip.IPv4Unspecified()
		attributes, err := syscall.ParseNetlinkRouteAttr(message)
		if err != nil {
			return nil, fmt.Errorf("parse netlink route attributes: %w", err)
		}
		for _, attribute := range attributes {
			switch attribute.Attr.Type {
			case syscall.RTA_DST:
				if parsed, ok := netip.AddrFromSlice(attribute.Value); ok {
					dst = parsed
				}
			case syscall.RTA_PREFSRC:
				if parsed, ok := netip.AddrFromSlice(attribute.Value); ok {
					route.PrefSrc = parsed
				}
			case syscall.RTA_TABLE:
				// The full table ID; rtm_table holds only eight bits.
				if len(attribute.Value) >= 4 {
					route.Table = binary.NativeEndian.Uint32(attribute.Value)
				}
			}
		}
		route.Dst = netip.PrefixFrom(dst, dstLen)
		routes = append(routes, route)
	}
	return routes, nil
}
