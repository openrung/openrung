package broker

import (
	"net/http"
	"net/netip"
	"strings"
)

// clientDenyList refuses requests whose resolved source address falls inside an
// operator-configured prefix. It is the control-plane counterpart to the
// per-IP rate limits: those bound what any one source may consume, while this
// answers the case where the operator has decided a source should consume
// nothing at all.
//
// The address it matches is the one clientIPResolver yields, so the list can
// only name what the broker can actually see. Behind a front that presents its
// own edge address, every client of that edge shares one source — so an
// operator must only list prefixes known to be a single caller, never a CDN
// edge, a carrier-grade NAT pool, or any other shared address.
type clientDenyList struct {
	prefixes []netip.Prefix
}

// newClientDenyList parses the configured CIDRs, skipping blank and
// unparseable entries. It returns nil when nothing is configured, which makes
// guard a pass-through: a broker with no deny list pays nothing for one.
func newClientDenyList(cidrs []string) *clientDenyList {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, raw := range cidrs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
			prefixes = append(prefixes, p.Masked())
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	return &clientDenyList{prefixes: prefixes}
}

// denies reports whether source is covered by the list. An unparseable source
// is never denied: the deny decision is deliberately conservative, since the
// cost of a wrong match is cutting off a real client.
func (d *clientDenyList) denies(source string) bool {
	if d == nil {
		return false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(source))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range d.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// guard wraps next so denied sources get 403 before the handler runs — so no
// work is done, no telemetry is synthesized, and no relay list is signed for
// them. The 403 is marked no-store: like the 429s, it is per-client state that
// an edge cache must never replay to everyone behind it.
func (d *clientDenyList) guard(clientIP *clientIPResolver, next http.HandlerFunc) http.HandlerFunc {
	if d == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if d.denies(clientIP.clientIP(r)) {
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	}
}
