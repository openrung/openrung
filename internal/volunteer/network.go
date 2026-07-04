package volunteer

import (
	"errors"
	"net"
)

var ErrNoGlobalIPv6Address = errors.New("no global unicast IPv6 address found")

func DefaultPublicIPv6Address() (string, error) {
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
			if isAdvertisableIPv6(ip) {
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

func isAdvertisableIPv6(ip net.IP) bool {
	return ip != nil &&
		ip.To4() == nil &&
		ip.IsGlobalUnicast() &&
		!ip.IsPrivate()
}
