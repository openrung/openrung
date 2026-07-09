package vpnservice

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/persist"
	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/relay"
)

// newManager builds a best-effort telemetry manager (parity with the mobile
// apps). A nil result means telemetry is unavailable; every call site guards
// for nil so connecting never fails on telemetry.
func newManager(brokerURL string) *clienttelemetry.Manager {
	if brokerURL == "" {
		brokerURL = config.TelemetryBrokerURL
	}
	mgr, err := clienttelemetry.New(brokerURL, client.AppVersion(), nil)
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

// selectRelay picks a relay: an exact relay id wins, else the first usable relay
// in the requested country, else the broker's first usable candidate. Freshness
// is judged against broker server time, like the CLI and mobile clients.
func selectRelay(resp relay.ListResponse, targetCountry, targetRelayID string) (relay.Descriptor, error) {
	now := resp.ServerTime
	if now.IsZero() {
		now = time.Now()
	}

	// Distinguish "broker returned nothing" up front from the narrower
	// no-match cases below, so telemetry can tell them apart.
	if len(resp.Relays) == 0 {
		return relay.Descriptor{}, client.ErrNoRelaysAvailable
	}

	if id := strings.TrimSpace(targetRelayID); id != "" {
		for _, candidate := range resp.Relays {
			if candidate.ID == id && client.IsUsableRelay(candidate, now) {
				return candidate, nil
			}
		}
		return relay.Descriptor{}, fmt.Errorf("relay %q: %w", id, client.ErrRelayNotInList)
	}

	if cc := strings.ToUpper(strings.TrimSpace(targetCountry)); cc != "" {
		for _, candidate := range resp.Relays {
			if strings.ToUpper(strings.TrimSpace(candidate.CountryCode)) == cc &&
				client.IsUsableRelay(candidate, now) {
				return candidate, nil
			}
		}
		return relay.Descriptor{}, fmt.Errorf("country %s: %w", cc, client.ErrNoRelayInCountry)
	}

	return client.SelectRelayForFamily(resp, client.RelayFamilyAuto)
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
// the volunteer's chosen label. It never returns a raw IP (contract §3).
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
