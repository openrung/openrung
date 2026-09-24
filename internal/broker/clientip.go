package broker

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIPResolver determines the real client IP of a request, honoring forwarded headers only when
// the request arrives from a trusted proxy. Trusted proxies come solely from operator configuration
// (OPENRUNG_TRUSTED_PROXY_CIDRS); with none configured, every request is keyed on its peer address,
// so a forged forwarded header cannot spoof the recorded source IP.
type clientIPResolver struct {
	trusted []netip.Prefix
}

// newClientIPResolver trusts exactly the operator-supplied CIDRs. Blank or unparseable CIDRs are
// skipped.
func newClientIPResolver(trustedCIDRs []string) *clientIPResolver {
	prefixes := make([]netip.Prefix, 0, len(trustedCIDRs))
	for _, raw := range trustedCIDRs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
			prefixes = append(prefixes, p.Masked())
		}
	}
	return &clientIPResolver{trusted: prefixes}
}

// clientIP returns the best-known client IP for r. When the immediate peer is a trusted proxy it
// honors CF-Connecting-IP, then the left-most X-Forwarded-For entry (both validated as IPs).
// Otherwise — or when no valid forwarded value is present — it returns the peer address parsed from
// RemoteAddr. Forwarded headers from an untrusted peer are ignored so they cannot spoof the source.
func (c *clientIPResolver) clientIP(r *http.Request) string {
	peer := peerIP(r.RemoteAddr)
	if peer == "" {
		return strings.TrimSpace(r.RemoteAddr)
	}
	if c.trusts(peer) {
		if cf := normalizedIP(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
		if fwd := leftmostForwarded(r.Header.Get("X-Forwarded-For")); fwd != "" {
			return fwd
		}
	}
	return peer
}

func (c *clientIPResolver) trusts(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range c.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// peerIP extracts the host portion of a RemoteAddr ("ip:port" or a bare "ip"), normalized, or "".
func peerIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return normalizedIP(host)
}

// normalizedIP validates s as an IP and returns its canonical string (v4-in-v6 unwrapped), or "".
func normalizedIP(s string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

// leftmostForwarded returns the first valid IP in a comma-separated X-Forwarded-For header, or "".
func leftmostForwarded(header string) string {
	for _, part := range strings.Split(header, ",") {
		if ip := normalizedIP(part); ip != "" {
			return ip
		}
	}
	return ""
}
