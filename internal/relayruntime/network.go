package relayruntime

import (
	"errors"
	"net"
	"net/netip"
	"strings"
)

var ErrNoGlobalIPv6Address = errors.New("no global unicast IPv6 address found")

// DefaultPublicIPv6Address returns the host's first global unicast IPv6
// address, skipping exclude (an IP literal, or "" for none): a relay with an
// access address passes it here, since that address sits on the same
// interface but must never become the advertised public host.
func DefaultPublicIPv6Address(exclude string) (string, error) {
	skip, _ := netip.ParseAddr(strings.TrimSpace(exclude))
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := addressIP(addr)
			if isAdvertisableIPv6(ip) && !sameIP(ip, skip) {
				return ip.String(), nil
			}
		}
	}
	return "", ErrNoGlobalIPv6Address
}

func addressIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func sameIP(ip net.IP, addr netip.Addr) bool {
	parsed, ok := netip.AddrFromSlice(ip)
	return ok && addr.IsValid() && parsed.Unmap() == addr.Unmap()
}

func isAdvertisableIPv6(ip net.IP) bool {
	return ip != nil &&
		ip.To4() == nil &&
		ip.IsGlobalUnicast() &&
		!ip.IsPrivate()
}
