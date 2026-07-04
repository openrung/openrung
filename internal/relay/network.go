package relay

import (
	"net"
	"strings"
)

func IsIPv6Host(host string) bool {
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}
