package connectcore

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/clienttelemetry"
)

// newManager builds a best-effort telemetry manager (parity with the mobile
// apps). A nil result means telemetry is unavailable; every call site guards
// for nil so connecting never fails on telemetry.
func (s *Engine) newManager(brokerURL string) *clienttelemetry.Manager {
	if brokerURL == "" {
		brokerURL = TelemetryBrokerURL
	}
	platform := s.telemetryPlatform()
	mgr, err := clienttelemetry.NewWithPlatform(
		brokerURL,
		client.AppVersion(),
		platform,
		s.brokerHTTPClient(),
	)
	if err != nil {
		return nil
	}
	// Desktop telemetry predates the platform attribute and must stay
	// byte-identical; every later platform stamps its label on each event so
	// dashboards can tell, say, a CLI session from a GUI session on one OS.
	if platform != brokerapi.PlatformDesktop {
		mgr.SetPlatformLabel(string(platform))
	}
	return mgr
}

// geoLookupTimeout is the hard backstop on the public-IP geo lookup, above
// LookupGeoAttributes' own 4s HTTP client timeout, for flows that never reach
// an abandon point.
const geoLookupTimeout = 5 * time.Second

// geoLookup tracks the in-flight public-IP geo lookup. Nothing ever waits on
// it — telemetry must never delay connecting or teardown; callers abandon it
// at the points where its result could no longer be trusted or used.
type geoLookup struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the lookup goroutine has finished (tests synchronize on it)
}

// abandon cancels an unfinished lookup without waiting for it. Nil-safe and
// idempotent. A lookup that already completed keeps its attached result: the
// response was fetched before the cancel, so it is the client's own geo.
func (g *geoLookup) abandon() {
	if g == nil {
		return
	}
	g.cancel()
}

// attachGeoAttributes resolves the client's public-IP geo in the background
// and attaches it to the session's telemetry (country/city/ISP on the broker
// dashboard, the way the mobile apps report it). Best-effort: events recorded
// before the lookup completes simply lack geo, and connect never waits on it.
// The caller must abandon the returned lookup before the tunnel can capture
// traffic — afterwards a still-running lookup would ride the tunnel and
// report the relay's geo instead of the client's.
func attachGeoAttributes(mgr *clienttelemetry.Manager, httpClient *http.Client) *geoLookup {
	if mgr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), geoLookupTimeout)
	g := &geoLookup{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(g.done)
		defer cancel()
		mgr.SetGeoAttributes(lookupGeoAttributes(ctx, httpClient))
	}()
	return g
}

func managerClientID(mgr *clienttelemetry.Manager) string {
	if mgr == nil {
		return ""
	}
	return mgr.ClientID()
}

// defaultFlushBudget bounds a terminal telemetry flush when no caller-owned
// budget was set (see Engine.Shutdown).
const defaultFlushBudget = 5 * time.Second

// terminalFlush drains the session's outbox once, bounded by the connection's
// flush budget (Shutdown's, else the default). The budget read and the cancel
// registration share one lock acquisition, so either Shutdown's budget is
// stored before the flush starts and bounds it directly, or Shutdown finds
// flushCancel and shortens the flush already in flight — a Shutdown can never
// be left waiting out an earlier, longer budget. The outcome lands in
// conn.flushErr for Shutdown to report.
func (s *Engine) terminalFlush(conn *connection) {
	if conn.mgr == nil {
		return
	}
	s.mu.Lock()
	budget := conn.flushBudget
	if budget <= 0 {
		budget = defaultFlushBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	conn.flushCancel = cancel
	s.mu.Unlock()
	defer cancel()
	err := conn.mgr.Flush(ctx)
	s.mu.Lock()
	conn.flushErr = err
	s.mu.Unlock()
}

// usableRelays filters the broker response to usable candidates, preserving
// broker order — the ordering carries the broker's ranking, so filtering must
// not disturb it (docs/api.md). Reordering it is a separate, deliberate step the
// ranker owns, on a signal the broker cannot see (see ranker.go); filtering
// itself stays order-preserving. Freshness is judged against broker server time,
// like the CLI and mobile clients.
func usableRelays(resp brokerapi.RelayListResponse) []brokerapi.RelayDescriptor {
	now := resp.ServerTime
	if now.IsZero() {
		now = time.Now()
	}
	usable := make([]brokerapi.RelayDescriptor, 0, len(resp.Relays))
	for _, candidate := range resp.Relays {
		if client.IsUsableRelay(candidate, now) {
			usable = append(usable, candidate)
		}
	}
	return usable
}

// RelayTarget narrows a candidate list to one connect target: an exact broker
// relay id, a friendly relay label (the CLI's -relay-label; several relays may
// share one), or a country. Identity targeting — id or label — wins over
// country; the zero value targets nothing and keeps the whole list.
type RelayTarget struct {
	RelayID string
	Label   string
	Country string
}

// Targeted reports whether the target narrows the list at all. A targeted
// connect fetches the full directory page, so the target is present even when
// the ranked default page would have missed it.
func (t RelayTarget) Targeted() bool {
	return t.identity() || strings.TrimSpace(t.Country) != ""
}

func (t RelayTarget) identity() bool {
	return strings.TrimSpace(t.RelayID) != "" || strings.TrimSpace(t.Label) != ""
}

// describe names the identity target for an error message.
func (t RelayTarget) describe() string {
	id, label := strings.TrimSpace(t.RelayID), strings.TrimSpace(t.Label)
	switch {
	case id != "" && label != "":
		return fmt.Sprintf("relay %q / label %q", id, label)
	case id != "":
		return fmt.Sprintf("relay %q", id)
	default:
		return fmt.Sprintf("label %q", label)
	}
}

// FilterCandidates narrows a candidate list to the connect target, mirroring the
// mobile targeting semantics: an identity target is pinned (never silently falls
// back to a different relay), a country keeps every candidate in it (geo-less
// relays are excluded so a targeted connect never lands elsewhere), no target
// keeps the whole list. Order is preserved — it carries the broker's ranking.
// The returned stage labels a failure for telemetry.
func FilterCandidates(candidates []brokerapi.RelayDescriptor, target RelayTarget) ([]brokerapi.RelayDescriptor, string, error) {
	if target.identity() {
		id, label := strings.TrimSpace(target.RelayID), strings.TrimSpace(target.Label)
		matched := make([]brokerapi.RelayDescriptor, 0, len(candidates))
		for _, candidate := range candidates {
			if (id != "" && candidate.ID == id) || (label != "" && candidate.Label == label) {
				matched = append(matched, candidate)
			}
		}
		if len(matched) == 0 {
			return nil, "relay_id_filter", fmt.Errorf("%s: %w", target.describe(), client.ErrRelayNotInList)
		}
		return matched, "", nil
	}

	if cc := strings.TrimSpace(target.Country); cc != "" {
		matched := make([]brokerapi.RelayDescriptor, 0, len(candidates))
		for _, candidate := range candidates {
			if strings.EqualFold(strings.TrimSpace(candidate.CountryCode), cc) {
				matched = append(matched, candidate)
			}
		}
		if len(matched) == 0 {
			return nil, "relay_geo_filter", fmt.Errorf("country %s: %w", strings.ToUpper(cc), client.ErrNoRelayInCountry)
		}
		return matched, "", nil
	}

	return candidates, "", nil
}

// demoteRelay moves the given relay to the end of the candidate list (order
// otherwise preserved): a relay that just failed is retried last, never
// excluded — it may be the only relay there is.
func demoteRelay(cands []brokerapi.RelayDescriptor, id string) []brokerapi.RelayDescriptor {
	reordered := make([]brokerapi.RelayDescriptor, 0, len(cands))
	var demoted []brokerapi.RelayDescriptor
	for _, cand := range cands {
		if cand.ID == id {
			demoted = append(demoted, cand)
			continue
		}
		reordered = append(reordered, cand)
	}
	return append(reordered, demoted...)
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
func geoLabel(r brokerapi.RelayDescriptor) string {
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
func recentFrom(r brokerapi.RelayDescriptor) *RecentNode {
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

// persistPrepend adds node to the front of recents (deduped, capped) and writes
// the result through Persistence, returning the new in-memory list.
func (s *Engine) persistPrepend(existing []RecentNode, node RecentNode) []RecentNode {
	recents := prependRecent(existing, node, MaxRecents)
	if s.Persistence != nil {
		_ = s.Persistence.SaveRecents(recents)
	}
	return recents
}

// prependRecent inserts node at the front, de-duplicated by countryCode, capped
// at max (matching the contract's cap-8 newest-first recents). It returns the
// new list so the caller can mirror it into state.
func prependRecent(existing []RecentNode, node RecentNode, max int) []RecentNode {
	out := make([]RecentNode, 0, len(existing)+1)
	out = append(out, node)
	for _, r := range existing {
		if r.CountryCode == node.CountryCode {
			continue
		}
		out = append(out, r)
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}
