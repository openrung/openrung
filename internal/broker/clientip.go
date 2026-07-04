package broker

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// cloudflareIPRanges is a snapshot of Cloudflare's published edge ranges
// (https://www.cloudflare.com/ips-v4 and https://www.cloudflare.com/ips-v6), captured 2026-06-30.
//
// A request whose immediate peer is in one of these ranges is trusted to carry the real client IP
// in its CF-Connecting-IP / X-Forwarded-For header (Cloudflare sets these for us). A request from
// anywhere else — notably a direct hit on the raw origin port, which stays open for the app's
// direct-IP fallback — is NOT trusted, so a forged forwarded header cannot spoof the recorded
// source IP. Refresh from the URLs above if Cloudflare changes its ranges; operators can also
// append ranges via OPENRUNG_TRUSTED_PROXY_CIDRS without a code change.
var cloudflareIPRanges = []string{
	// IPv4
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	// IPv6
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// clientIPResolver determines the real client IP of a request, honoring forwarded headers only when
// the request arrives from a trusted proxy (Cloudflare by default).
type clientIPResolver struct {
	trusted []netip.Prefix
}

// newClientIPResolver trusts Cloudflare's published ranges plus any operator-supplied CIDRs.
// Blank or unparseable CIDRs are skipped.
func newClientIPResolver(extraCIDRs []string) *clientIPResolver {
	all := make([]string, 0, len(cloudflareIPRanges)+len(extraCIDRs))
	all = append(all, cloudflareIPRanges...)
	all = append(all, extraCIDRs...)

	prefixes := make([]netip.Prefix, 0, len(all))
	for _, raw := range all {
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
