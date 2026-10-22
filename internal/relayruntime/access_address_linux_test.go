// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package relayruntime

import (
	"encoding/binary"
	"net/netip"
	"syscall"
	"testing"
)

// rtNewAddr builds one RTM_NEWADDR message as the kernel sends it: an
// ifaddrmsg followed by an IFA_ADDRESS and, when extended is non-zero, an
// IFA_FLAGS attribute.
func rtNewAddr(family byte, ip netip.Addr, flags byte, extended uint32) []byte {
	attr := func(kind uint16, value []byte) []byte {
		length := syscall.SizeofRtAttr + len(value)
		out := make([]byte, (length+3)&^3)
		binary.NativeEndian.PutUint16(out[0:2], uint16(length))
		binary.NativeEndian.PutUint16(out[2:4], kind)
		copy(out[syscall.SizeofRtAttr:], value)
		return out
	}
	body := []byte{family, 64, flags, 0, 2, 0, 0, 0}
	body = append(body, attr(syscall.IFA_ADDRESS, ip.AsSlice())...)
	if extended != 0 {
		value := make([]byte, 4)
		binary.NativeEndian.PutUint32(value, extended)
		body = append(body, attr(ifaFlags, value)...)
	}
	header := make([]byte, syscall.NLMSG_HDRLEN)
	binary.NativeEndian.PutUint32(header[0:4], uint32(len(header)+len(body)))
	binary.NativeEndian.PutUint16(header[4:6], syscall.RTM_NEWADDR)
	return append(header, body...)
}

func TestParseIPv6AddressMessagesReadsFlags(t *testing.T) {
	primary := netip.MustParseAddr("2001:db8:1::1")
	access := netip.MustParseAddr("2001:db8:1::10")
	tentative := netip.MustParseAddr("2001:db8:1::20")
	var rib []byte
	rib = append(rib, rtNewAddr(syscall.AF_INET6, primary, 0, 0)...)
	// The deprecated bit arrives in IFA_FLAGS, which wins over ifa_flags.
	rib = append(rib, rtNewAddr(syscall.AF_INET6, access, 0, syscall.IFA_F_DEPRECATED|0x100)...)
	rib = append(rib, rtNewAddr(syscall.AF_INET6, tentative, syscall.IFA_F_TENTATIVE, 0)...)
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, err := parseIPv6AddressMessages(messages)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []HostAddress{
		{IP: primary},
		{IP: access, Deprecated: true},
		{IP: tentative, Tentative: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d addresses %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("address %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The real dump runs unprivileged and always lists at least ::1 where IPv6
// is enabled; this proves the query works without capabilities.
func TestHostIPv6AddressesRunsUnprivileged(t *testing.T) {
	addresses, err := HostIPv6Addresses()
	if err != nil {
		t.Fatalf("HostIPv6Addresses: %v", err)
	}
	for _, address := range addresses {
		if !address.IP.Is6() {
			t.Fatalf("non-IPv6 address in the dump: %+v", address)
		}
	}
}
