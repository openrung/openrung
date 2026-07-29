package broker

import (
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"openrung/internal/relay"
)

//go:embed relays.html
var relaysHTML []byte

// maxOfflineRelayRows caps how many telemetry-only (offline) relays one
// response lists, keeping the page bounded even when the attested history is
// wide — e.g. identity-churn periods minted dozens of IDs per relay per week.
// The busiest rows survive; the totals still count every offline relay, and
// the page notes how many rows the cap hid. Online rows are never capped:
// they mirror the live registry, which expires every unrenewed lease within
// minutes.
const maxOfflineRelayRows = 200

// relayDirectoryLister is the one slice of RelayStore the relays panel reads:
// the currently registered descriptor set. Narrow so tests can fake it.
type relayDirectoryLister interface {
	List(time.Time, int) ([]relay.Descriptor, error)
}

// relayTelemetryStats is the telemetry half of the admin relays page: one row
// per broker-attested relay ID named by any event in the window, plus the
// overall distinct count of clients currently active through such a relay.
// That total cannot be derived from the rows — one client holding sessions on
// two relays would be counted twice by summing per-relay counts.
//
// Attested means at least one of the window's records retained a non-empty
// relay_node_class, which ingestion stamps only while the named relay holds a
// live registration. The anonymous telemetry API accepts any relay_id string,
// so without this gate a client could mint one row (and one "offline relay")
// per fabricated ID — up to 200 per accepted request — and grow the response
// without bound for the retention window. Registration is the trust anchor:
// it is separately rate limited, logged, and the only way to obtain a stamp.
type relayTelemetryStats struct {
	Relays        []relayStatRow
	ActiveClients int
}

// relayStatRow aggregates one relay's telemetry over the dashboard window.
// Sessions are attributed to the relay named by their latest relay-bearing
// event — the sessions endpoint and active_by_relay panel work the same way —
// so a session that failed over counts toward the relay it ended up on.
type relayStatRow struct {
	RelayID string `json:"relay_id"`
	// NodeClass is the broker-attested class retained with the relay's
	// telemetry (latest non-empty wins). It outlives the short relay lease,
	// so offline relays keep their class for the full retention window.
	NodeClass        string    `json:"node_class,omitempty"`
	Successes        int       `json:"successes"`
	Failures         int       `json:"failures"`
	TopFailureReason string    `json:"top_failure_reason,omitempty"`
	Sessions         int       `json:"sessions"`
	Clients          int       `json:"clients"`
	ActiveSessions   int       `json:"active_sessions"`
	ActiveClients    int       `json:"active_clients"`
	SpeedTests       int       `json:"speed_tests"`
	AverageMbps      float64   `json:"average_mbps"`
	AverageTTFBMS    float64   `json:"average_ttfb_ms"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// buildRelayTelemetryStats aggregates stored records per relay. It mirrors
// telemetryRelayStatsQuery; TestPostgresTelemetryQuerierMatchesInMemoryRelayStats
// holds the two implementations together.
func buildRelayTelemetryStats(records []TelemetryRecord, now time.Time, window time.Duration) relayTelemetryStats {
	start := now.Add(-window)
	type sessionState struct {
		clientID        string
		relayID         string
		lastHeartbeatAt time.Time
		terminal        bool
	}
	sessions := make(map[string]*sessionState)
	rows := make(map[string]*relayStatRow)
	reasons := make(map[string]map[string]int)
	type speedSums struct{ mbpsMilli, ttfbMS int64 }
	speeds := make(map[string]*speedSums)

	rowFor := func(id string) *relayStatRow {
		row := rows[id]
		if row == nil {
			row = &relayStatRow{RelayID: id}
			rows[id] = row
		}
		return row
	}

	for _, record := range records {
		event := record.Event
		if event.OccurredAt.Before(start) || event.OccurredAt.After(now) {
			continue
		}
		if event.Event == telemetryAppConnectionEvent {
			continue
		}
		if event.RelayID != "" {
			row := rowFor(event.RelayID)
			if record.ReceivedAt.After(row.LastSeenAt) {
				row.LastSeenAt = record.ReceivedAt
			}
			// Latest non-empty wins, like the relay_classes aggregation the
			// overview retains classes with.
			if record.RelayNodeClass != "" {
				row.NodeClass = record.RelayNodeClass
			}
		}
		// An empty session ID keys one shared session, exactly like the SQL
		// sessions CTE and buildTelemetryOverview group it.
		session := sessions[event.SessionID]
		if session == nil {
			session = &sessionState{clientID: event.ClientID}
			sessions[event.SessionID] = session
		}
		if event.RelayID != "" {
			session.relayID = event.RelayID
		}
		switch event.Event {
		case "session_heartbeat":
			if record.ReceivedAt.After(session.lastHeartbeatAt) {
				session.lastHeartbeatAt = record.ReceivedAt
			}
		case "connection_failed", "connection_ended", "tunnel_stopped":
			session.terminal = true
		}
		switch event.Event {
		case "connection_succeeded", "relay_failover":
			if event.RelayID != "" {
				rowFor(event.RelayID).Successes++
			}
		case "relay_attempt_failed":
			if event.RelayID != "" {
				rowFor(event.RelayID).Failures++
				reason := firstNonEmpty(event.Attributes["failure_reason"], event.Attributes["error_type"], "unknown")
				if reasons[event.RelayID] == nil {
					reasons[event.RelayID] = make(map[string]int)
				}
				reasons[event.RelayID][reason]++
			}
		case "speed_test_completed":
			if event.RelayID != "" {
				rowFor(event.RelayID).SpeedTests++
				sums := speeds[event.RelayID]
				if sums == nil {
					sums = &speedSums{}
					speeds[event.RelayID] = sums
				}
				sums.mbpsMilli += event.Measurements["download_mbps_milli"]
				sums.ttfbMS += event.Measurements["time_to_first_byte_ms"]
			}
		}
	}

	stats := relayTelemetryStats{}
	activeClients := make(map[string]struct{})
	relayClients := make(map[string]map[string]struct{})
	relayActiveClients := make(map[string]map[string]struct{})
	for _, session := range sessions {
		if session.relayID == "" {
			continue
		}
		// The row already exists: session.relayID was copied from an in-window
		// relay-bearing event, and that same event created the row above.
		row := rowFor(session.relayID)
		row.Sessions++
		addMember(relayClients, session.relayID, session.clientID)
		if !session.terminal && session.lastHeartbeatAt.After(now.Add(-activeSessionTimeout)) {
			row.ActiveSessions++
			addMember(relayActiveClients, session.relayID, session.clientID)
			// The fleet-wide connected-clients count trusts attested relays
			// only, or fabricated heartbeats would inflate it.
			if row.NodeClass != "" {
				activeClients[session.clientID] = struct{}{}
			}
		}
	}
	for id, row := range rows {
		// Unattested IDs aggregate above (a session's latest relay-bearing
		// event may name one) but are dropped here, on both backends alike.
		if row.NodeClass == "" {
			continue
		}
		row.Clients = len(relayClients[id])
		row.ActiveClients = len(relayActiveClients[id])
		row.TopFailureReason = topFailureReason(reasons[id])
		if sums := speeds[id]; sums != nil {
			applyRelaySpeedAverages(row, sums.mbpsMilli, sums.ttfbMS)
		}
		stats.Relays = append(stats.Relays, *row)
	}
	stats.ActiveClients = len(activeClients)
	sortRelayStatRows(stats.Relays)
	return stats
}

// applyRelaySpeedAverages derives the row's speed averages from the window's
// sums. Shared by both queriers so float rounding is identical.
func applyRelaySpeedAverages(row *relayStatRow, mbpsMilliSum, ttfbMSSum int64) {
	if row.SpeedTests <= 0 {
		return
	}
	row.AverageMbps = float64(mbpsMilliSum) / float64(row.SpeedTests) / 1000
	row.AverageTTFBMS = float64(ttfbMSSum) / float64(row.SpeedTests)
}

// sortRelayStatRows orders busiest-first — active sessions, then window
// sessions, then attempt volume — with the relay ID as the deterministic final
// tiebreak. Shared by both queriers so parity holds byte-for-byte.
func sortRelayStatRows(rows []relayStatRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ActiveSessions != b.ActiveSessions {
			return a.ActiveSessions > b.ActiveSessions
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		if volumeA, volumeB := a.Successes+a.Failures, b.Successes+b.Failures; volumeA != volumeB {
			return volumeA > volumeB
		}
		return a.RelayID < b.RelayID
	})
}

func addMember(sets map[string]map[string]struct{}, key, member string) {
	if sets[key] == nil {
		sets[key] = make(map[string]struct{})
	}
	sets[key][member] = struct{}{}
}

type relaysPanelResponse struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Window      string           `json:"window"`
	Totals      relayPanelTotals `json:"totals"`
	Relays      []relayPanelRow  `json:"relays"`
}

// relayPanelTotals summarizes the fleet. The registry counts (online,
// foundation/volunteer, capacity, transports, capabilities) cover currently
// registered relays; OfflineRelays counts relays that appear only in the
// window's telemetry. ConnectedClients is the distinct count of clients
// currently active through any relay, and ActiveSessions the sum of live
// sessions across every row (offline rows included: clients can outlive their
// relay's broker registration).
type relayPanelTotals struct {
	OnlineRelays       int `json:"online_relays"`
	FoundationRelays   int `json:"foundation_relays"`
	VolunteerRelays    int `json:"volunteer_relays"`
	OfflineRelays      int `json:"offline_relays"`
	ConnectedClients   int `json:"connected_clients"`
	ActiveSessions     int `json:"active_sessions"`
	SessionCapacity    int `json:"session_capacity"`
	DirectRelays       int `json:"direct_relays"`
	TunnelRelays       int `json:"tunnel_relays"`
	PunchCapableRelays int `json:"punch_capable_relays"`
	WSSCapableRelays   int `json:"wss_capable_relays"`
}

// relayPanelRow is one relay on the admin relays page: the live registry
// descriptor merged with the window's telemetry aggregates. Offline relays —
// seen in telemetry but not currently registered — carry only the telemetry
// half plus the retained node class.
type relayPanelRow struct {
	RelayID   string `json:"relay_id"`
	Label     string `json:"label,omitempty"`
	NodeClass string `json:"node_class,omitempty"`
	Online    bool   `json:"online"`
	// Registry fields, present on online rows only. Endpoint is the advertised
	// public host:port — for tunnel relays that is the relay hub, never the
	// operator's own address — and the geo fields locate the relay's exit.
	Endpoint        string     `json:"endpoint,omitempty"`
	Transport       string     `json:"transport,omitempty"`
	City            string     `json:"city,omitempty"`
	Country         string     `json:"country,omitempty"`
	CountryCode     string     `json:"country_code,omitempty"`
	RelayVersion    string     `json:"relay_version,omitempty"`
	MaxSessions     int        `json:"max_sessions,omitempty"`
	MaxMbps         int        `json:"max_mbps,omitempty"`
	PunchCapable    bool       `json:"punch_capable,omitempty"`
	WSSFronts       int        `json:"wss_fronts,omitempty"`
	RegisteredAt    *time.Time `json:"registered_at,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	// Telemetry fields, aggregated over the dashboard window.
	ActiveSessions   int        `json:"active_sessions"`
	ActiveClients    int        `json:"active_clients"`
	Sessions         int        `json:"sessions"`
	Clients          int        `json:"clients"`
	Successes        int        `json:"successes"`
	Failures         int        `json:"failures"`
	TopFailureReason string     `json:"top_failure_reason,omitempty"`
	SpeedTests       int        `json:"speed_tests,omitempty"`
	AverageMbps      float64    `json:"average_mbps,omitempty"`
	AverageTTFBMS    float64    `json:"average_ttfb_ms,omitempty"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
}

// buildRelaysPanel merges the live relay directory with the window's per-relay
// telemetry. Every registered relay appears, zero telemetry included; relays
// with telemetry but no current registration appear as offline rows, so a
// crashed or expired relay stays visible for the retention window. An online
// row's broker-attested descriptor class is authoritative; offline rows keep
// the class retained with their telemetry.
func buildRelaysPanel(descriptors []relay.Descriptor, stats relayTelemetryStats, now time.Time, window time.Duration) relaysPanelResponse {
	response := relaysPanelResponse{GeneratedAt: now, Window: window.String(), Relays: []relayPanelRow{}}
	statRows := make(map[string]relayStatRow, len(stats.Relays))
	for _, row := range stats.Relays {
		statRows[row.RelayID] = row
	}

	online := make(map[string]struct{}, len(descriptors))
	for _, desc := range descriptors {
		online[desc.ID] = struct{}{}
		row := relayPanelRow{
			RelayID:         desc.ID,
			Label:           desc.Label,
			NodeClass:       desc.NodeClass,
			Online:          true,
			Endpoint:        net.JoinHostPort(desc.PublicHost, strconv.Itoa(desc.PublicPort)),
			Transport:       desc.Transport,
			City:            desc.City,
			Country:         desc.Country,
			CountryCode:     desc.CountryCode,
			RelayVersion:    desc.RelayVersion,
			MaxSessions:     desc.MaxSessions,
			MaxMbps:         desc.MaxMbps,
			PunchCapable:    desc.PunchCapable,
			WSSFronts:       len(desc.WSSFronts),
			RegisteredAt:    timePointer(desc.RegisteredAt),
			LastHeartbeatAt: timePointer(desc.LastHeartbeatAt),
		}
		applyRelayStatRow(&row, statRows[desc.ID])

		response.Totals.OnlineRelays++
		if desc.NodeClass == relay.NodeClassFoundation {
			response.Totals.FoundationRelays++
		} else {
			response.Totals.VolunteerRelays++
		}
		response.Totals.SessionCapacity += desc.MaxSessions
		if desc.Transport == relay.TransportTunnel {
			response.Totals.TunnelRelays++
		} else {
			response.Totals.DirectRelays++
		}
		if desc.PunchCapable {
			response.Totals.PunchCapableRelays++
		}
		if len(desc.WSSFronts) > 0 {
			response.Totals.WSSCapableRelays++
		}
		response.Relays = append(response.Relays, row)
	}

	var offlineRows []relayPanelRow
	for _, statRow := range stats.Relays {
		if _, ok := online[statRow.RelayID]; ok {
			continue
		}
		row := relayPanelRow{RelayID: statRow.RelayID, NodeClass: statRow.NodeClass}
		applyRelayStatRow(&row, statRow)
		// Totals cover every offline relay; the row cap below bounds only what
		// the response lists.
		response.Totals.OfflineRelays++
		response.Totals.ActiveSessions += row.ActiveSessions
		offlineRows = append(offlineRows, row)
	}
	sortRelayPanelRows(offlineRows)
	if len(offlineRows) > maxOfflineRelayRows {
		offlineRows = offlineRows[:maxOfflineRelayRows]
	}

	for _, row := range response.Relays {
		response.Totals.ActiveSessions += row.ActiveSessions
	}
	response.Relays = append(response.Relays, offlineRows...)
	response.Totals.ConnectedClients = stats.ActiveClients
	sortRelayPanelRows(response.Relays)
	return response
}

// applyRelayStatRow copies the telemetry half onto a panel row. The retained
// telemetry class fills the class only when the row has none: an online row's
// broker-attested descriptor class always wins.
func applyRelayStatRow(row *relayPanelRow, stats relayStatRow) {
	row.ActiveSessions = stats.ActiveSessions
	row.ActiveClients = stats.ActiveClients
	row.Sessions = stats.Sessions
	row.Clients = stats.Clients
	row.Successes = stats.Successes
	row.Failures = stats.Failures
	row.TopFailureReason = stats.TopFailureReason
	row.SpeedTests = stats.SpeedTests
	row.AverageMbps = stats.AverageMbps
	row.AverageTTFBMS = stats.AverageTTFBMS
	row.LastSeenAt = timePointer(stats.LastSeenAt)
	if row.NodeClass == "" {
		row.NodeClass = stats.NodeClass
	}
}

// sortRelayPanelRows orders the page: online relays first, then the busiest
// (active sessions, window sessions, attempt volume), with the relay ID as
// the deterministic final tiebreak.
func sortRelayPanelRows(rows []relayPanelRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Online != b.Online {
			return a.Online
		}
		if a.ActiveSessions != b.ActiveSessions {
			return a.ActiveSessions > b.ActiveSessions
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		if volumeA, volumeB := a.Successes+a.Failures, b.Successes+b.Failures; volumeA != volumeB {
			return volumeA > volumeB
		}
		return a.RelayID < b.RelayID
	})
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func (d *dashboardServer) relaysPanel(w http.ResponseWriter, r *http.Request) {
	window, ok := dashboardWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be 1h, 24h, or 7d")
		return
	}
	now := d.now().UTC()
	stats, err := d.querier.TelemetryRelayStats(now, window)
	if err != nil {
		slog.Error("could not build relay telemetry stats", "error", err)
		writeError(w, http.StatusInternalServerError, "could not build relay stats")
		return
	}
	var descriptors []relay.Descriptor
	if d.relayDirectory != nil {
		descriptors, err = d.relayDirectory.List(now, 0)
		if err != nil {
			slog.Error("could not list relays for dashboard", "error", err)
			writeError(w, http.StatusInternalServerError, "could not list relays")
			return
		}
	}
	writeJSON(w, http.StatusOK, buildRelaysPanel(descriptors, stats, now, window))
}
