package vpnservice

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/persist"
	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/relay"
)

// telemetryHTTPTimeout bounds every telemetry request. Without it the manager
// falls back to http.DefaultClient (no timeout), so a broker that accepts the
// connection but stalls the response would hang the synchronous Flush on the
// connect path — and, since Flush runs before supervision starts, disable
// mid-session recovery. Matches the mobile client's 15s request deadline.
const telemetryHTTPTimeout = 15 * time.Second

// newManager builds a best-effort telemetry manager (parity with the mobile
// apps). A nil result means telemetry is unavailable; every call site guards
// for nil so connecting never fails on telemetry.
func newManager(brokerURL string) *clienttelemetry.Manager {
	if brokerURL == "" {
		brokerURL = config.TelemetryBrokerURL
	}
	mgr, err := clienttelemetry.New(brokerURL, client.AppVersion(), &http.Client{Timeout: telemetryHTTPTimeout})
	if err != nil {
		return nil
	}
	return mgr
}

func managerClientID(mgr *clienttelemetry.Manager) string {
	if mgr == nil {
		return ""
	}
	return mgr.ClientID()
}

func endSession(mgr *clienttelemetry.Manager, reason string) {
	if mgr == nil {
		return
	}
	mgr.EndSession(reason)
	flushOnShutdown(mgr)
}

// flushOnShutdown flushes remaining telemetry with a fresh bounded context, so
// it still runs after the connect context has been cancelled.
func flushOnShutdown(mgr *clienttelemetry.Manager) {
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mgr.Flush(ctx)
}

// usableRelays filters the broker response to usable candidates, preserving
// broker order — the ordering IS the broker's ranking signal, so clients filter
// without re-sorting (docs/api.md). Freshness is judged against broker server
// time, like the CLI and mobile clients.
func usableRelays(resp relay.ListResponse) []relay.Descriptor {
	now := resp.ServerTime
	if now.IsZero() {
		now = time.Now()
	}
	usable := make([]relay.Descriptor, 0, len(resp.Relays))
	for _, candidate := range resp.Relays {
		if client.IsUsableRelay(candidate, now) {
			usable = append(usable, candidate)
		}
	}
	return usable
}

// filterCandidates narrows the usable list to the connect target, mirroring the
// mobile targeting semantics: an exact relay id is pinned (never silently falls
// back to a different relay), a country keeps every usable relay in it (geo-less
// relays are excluded so a targeted connect never lands elsewhere), no target
// keeps the whole list. The returned stage labels a failure for telemetry.
func filterCandidates(usable []relay.Descriptor, targetCountry, targetRelayID string) ([]relay.Descriptor, string, error) {
	if id := strings.TrimSpace(targetRelayID); id != "" {
		matched := make([]relay.Descriptor, 0, 1)
		for _, candidate := range usable {
			if candidate.ID == id {
				matched = append(matched, candidate)
			}
		}
		if len(matched) == 0 {
			return nil, "relay_id_filter", fmt.Errorf("relay %q: %w", id, client.ErrRelayNotInList)
		}
		return matched, "", nil
	}

	if cc := strings.TrimSpace(targetCountry); cc != "" {
		matched := make([]relay.Descriptor, 0, len(usable))
		for _, candidate := range usable {
			if strings.EqualFold(strings.TrimSpace(candidate.CountryCode), cc) {
				matched = append(matched, candidate)
			}
		}
		if len(matched) == 0 {
			return nil, "relay_geo_filter", fmt.Errorf("country %s: %w", strings.ToUpper(cc), client.ErrNoRelayInCountry)
		}
		return matched, "", nil
	}

	return usable, "", nil
}

// demoteRelay moves the given relay to the end of the candidate list (order
// otherwise preserved): a relay that just failed is retried last, never
// excluded — it may be the only relay there is.
func demoteRelay(cands []relay.Descriptor, id string) []relay.Descriptor {
	reordered := make([]relay.Descriptor, 0, len(cands))
	var demoted []relay.Descriptor
	for _, cand := range cands {
		if cand.ID == id {
			demoted = append(demoted, cand)
			continue
		}
		reordered = append(reordered, cand)
	}
	return append(reordered, demoted...)
}

// freeLoopbackPort returns an unused loopback TCP port for the mixed inbound.
// There is a small TOCTOU window before sing-box binds; acceptable for a
// user-launched desktop connect.
func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func writeTempConfig(data []byte) (string, error) {
	file, err := os.CreateTemp("", "openrung-proxy-*.json")
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// geoLabel is the user-facing relay label: "City, Country", else country, else
// the relay's friendly label. It never returns a raw IP (contract §3).
func geoLabel(r relay.Descriptor) string {
	city := strings.TrimSpace(r.City)
	country := strings.TrimSpace(r.Country)
	switch {
	case city != "" && country != "":
		return city + ", " + country
	case country != "":
		return country
	case strings.TrimSpace(r.Label) != "":
		return strings.TrimSpace(r.Label)
	default:
		return "relay " + r.ID
	}
}

// recentFrom builds a RecentNode from a relay's broker-served geo. Returns nil
// when the relay has no country code (nothing tap-to-connect could target).
func recentFrom(r relay.Descriptor) *RecentNode {
	cc := strings.ToUpper(strings.TrimSpace(r.CountryCode))
	if cc == "" {
		return nil
	}
	return &RecentNode{
		CountryCode: cc,
		Label:       geoLabel(r),
		Latitude:    r.Latitude,
		Longitude:   r.Longitude,
	}
}

func toRecentNodes(stored []persist.RecentNode) []RecentNode {
	out := make([]RecentNode, 0, len(stored))
	for _, r := range stored {
		out = append(out, RecentNode(r))
	}
	return out
}

func toStoredRecents(nodes []RecentNode) []persist.RecentNode {
	out := make([]persist.RecentNode, 0, len(nodes))
	for _, r := range nodes {
		out = append(out, persist.RecentNode(r))
	}
	return out
}

// persistPrepend adds node to the front of recents (deduped, capped) and writes
// the result through, returning the new in-memory list.
func persistPrepend(store *persist.Store, existing []RecentNode, node RecentNode) []RecentNode {
	stored := persist.PrependRecent(toStoredRecents(existing), persist.RecentNode(node), config.MaxRecents)
	if store != nil {
		_ = store.SaveRecents(stored)
	}
	return toRecentNodes(stored)
}
