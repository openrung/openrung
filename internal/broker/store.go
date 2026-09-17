package broker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"sync"
	"time"

	"openrung/internal/relay"
)

var ErrRelayNotFound = errors.New("relay not found")

// ErrNodeClassForbidden is returned when a registration or heartbeat would
// act on a foundation relay without foundation authority: a non-foundation
// registration cannot seize a foundation endpoint (Register), and a
// non-foundation credential cannot extend a foundation lease (Heartbeat).
// Refusing outright (rather than downgrading the row) keeps a foundation
// label from being taken over — public relay IDs and endpoints notwithstanding
// — and lets an orphaned label expire within one lease TTL.
var ErrNodeClassForbidden = errors.New("registration or heartbeat is not authorized for this relay's node class")

// ErrInvalidRankingWeight is returned by the ranking-weight setters for a
// value outside [0, 1] (or NaN). The stores validate rather than clamp so an
// operator's typo surfaces as an error instead of silently draining a relay.
var ErrInvalidRankingWeight = errors.New("ranking weight must be a number between 0 and 1")

// validateRankingWeight is the single range check both stores apply before
// persisting an operator ranking weight.
func validateRankingWeight(weight float64) error {
	if math.IsNaN(weight) || weight < 0 || weight > 1 {
		return ErrInvalidRankingWeight
	}
	return nil
}

type RankingMode string

const (
	RankingModeGlobal RankingMode = "global"
	RankingModeLegacy RankingMode = "legacy"
)

type RelayStore interface {
	Register(relay.RegisterRequest, time.Time, time.Duration) (relay.Descriptor, error)
	// Heartbeat extends a relay's lease. maxClass is the highest node class
	// the caller's credential vouches for: extending a foundation relay's
	// lease requires a foundation credential (ErrNodeClassForbidden
	// otherwise), so a foundation label that lost its authorized registrant —
	// e.g. an endpoint takeover through a rolled-back broker binary whose
	// upsert predates node_class — expires within one TTL instead of being
	// kept alive indefinitely by whoever now heartbeats the ID.
	Heartbeat(id, leaseToken, maxClass string, now time.Time, ttl time.Duration) (relay.Descriptor, error)
	UpdateGeo(id, leaseToken string, geo relay.GeoLocation) error
	List(time.Time, int) ([]relay.Descriptor, error)
	// ListRanked is List plus the operator ranking weights the ranking applied,
	// so page-shaping steps after the sort (the WSS reservation) and the
	// operator views can honour the same weights without a second read.
	ListRanked(time.Time, int) ([]relay.Descriptor, map[string]float64, error)
	// RelayByID resolves one currently active descriptor for relay-bound ticket
	// issuance. It must never return an expired row.
	RelayByID(string, time.Time) (relay.Descriptor, error)
	// RelayNodeClasses resolves active relay IDs to the broker-attested class
	// stored on their descriptors. Telemetry ingestion uses this bounded lookup
	// so the class can be retained with historical events after the short relay
	// lease expires, without trusting a client-supplied class.
	RelayNodeClasses(context.Context, []string, time.Time) (map[string]string, error)
	Stats(time.Time) (StoreStats, error)
	Prune(time.Time) ([]relay.Descriptor, error)
	RecordRelayTelemetry(context.Context, []TelemetryRecord, time.Time) error
	// RelayRankingWeights returns every stored operator ranking-weight
	// override, keyed by relay ID. Relays absent from the map rank at
	// defaultRankingWeight. Overrides are keyed by the identity-derived relay
	// ID and are never pruned with descriptors, so a weight set on a relay
	// survives its lease expiring and its next re-registration.
	RelayRankingWeights(context.Context) (map[string]float64, error)
	// SetRelayRankingWeight stores weight for id — ErrInvalidRankingWeight
	// unless it lies in [0, 1] — and returns the effective weight it replaced
	// (defaultRankingWeight when the relay had no override). The relay need
	// not be registered: an operator may pre-drain a relay that is currently
	// between leases.
	SetRelayRankingWeight(context.Context, string, float64) (float64, error)
	// DeleteRelayRankingWeight removes id's override, restoring the default,
	// and returns the effective weight it replaced. Deleting an absent
	// override is a successful no-op that returns defaultRankingWeight.
	DeleteRelayRankingWeight(context.Context, string) (float64, error)
	Ping(context.Context) error
	Close() error
}

type Store struct {
	mu           sync.RWMutex
	relays       map[string]relay.Descriptor
	sessions     map[string]relaySessionState
	observations []relayMetricObservation
	// rankingWeights holds operator overrides only; see RelayRankingWeights.
	rankingWeights map[string]float64
	rankingMode    RankingMode
}

type StoreStats struct {
	// ActiveRelays counts every unexpired foundation- and volunteer-class relay.
	ActiveRelays              int
	AdvertisedSessionCapacity int
}

func NewStore() *Store {
	return NewStoreWithRanking(RankingModeGlobal)
}

func NewStoreWithRanking(rankingMode RankingMode) *Store {
	return &Store{
		relays:         make(map[string]relay.Descriptor),
		sessions:       make(map[string]relaySessionState),
		rankingWeights: make(map[string]float64),
		rankingMode:    normalizeRankingMode(rankingMode),
	}
}

func (s *Store) Register(req relay.RegisterRequest, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	// Verification lives in the store, not the handler, so no future caller
	// can register an identity-bearing request without the possession proof —
	// the derived ID is only ever handed to a registrant holding the private
	// key. Legacy requests (no identity fields) return a nil key and keep the
	// random mint below.
	identityKey, err := relay.VerifyIdentity(req, now)
	if err != nil {
		return relay.Descriptor{}, err
	}
	if err := relay.VerifyWSSCapability(req, now); err != nil {
		return relay.Descriptor{}, err
	}
	var id string
	if identityKey != nil {
		id = relay.DeriveRelayID(identityKey)
	} else if id, err = newRelayID(); err != nil {
		return relay.Descriptor{}, err
	}
	var leaseToken string
	if identityKey != nil {
		leaseToken, err = newRelayLeaseToken()
		if err != nil {
			return relay.Descriptor{}, err
		}
	}

	desc := relay.Descriptor{
		ID:                id,
		IdentityPublicKey: req.IdentityPublicKey,
		LeaseToken:        leaseToken,
		Label:             req.Label,
		NodeClass:         normalizeNodeClass(req.NodeClass),
		PublicHost:        req.PublicHost,
		PublicPort:        req.PublicPort,
		ExitHost:          req.ExitHost,
		Protocol:          req.Protocol,
		ClientID:          req.ClientID,
		RealityPublicKey:  req.RealityPublicKey,
		ShortID:           req.ShortID,
		ServerName:        req.ServerName,
		Flow:              req.Flow,
		ExitMode:          req.ExitMode,
		MaxSessions:       req.MaxSessions,
		MaxMbps:           req.MaxMbps,
		RelayVersion:      req.RelayVersion,
		Transport:         normalizeTransport(req.Transport),
		PunchCapable:      req.PunchCapable,
		PunchEndpoint:     req.PunchEndpoint,
		WSSFronts:         append([]relay.WSSFrontDescriptor(nil), req.WSSFronts...),
		RegisteredAt:      now,
		LastHeartbeatAt:   now,
		ExpiresAt:         now.Add(ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Mirror the postgres endpoint upsert: each public host:port identifies one
	// current descriptor, and a successful re-registration replaces the old ID.
	// Validate every matching row before deleting any of them so a legacy store
	// containing duplicate rows cannot be partially modified when one of those
	// rows is a live foundation relay.
	replacedIDs := make([]string, 0, 1)
	var preservedGeo relay.GeoLocation
	var preservedGeoRegisteredAt time.Time
	hasPreservedGeo := false
	for existingID, existing := range s.relays {
		if existing.PublicHost != desc.PublicHost || existing.PublicPort != desc.PublicPort {
			continue
		}
		if desc.NodeClass != relay.NodeClassFoundation &&
			existing.NodeClass == relay.NodeClassFoundation &&
			existing.ExpiresAt.After(now) {
			return relay.Descriptor{}, ErrNodeClassForbidden
		}
		replacedIDs = append(replacedIDs, existingID)
		// Match postgres registration semantics: retain a resolved location
		// while the exit is unchanged, but clear it when an endpoint is reused
		// by a relay with a different exit host.
		if existing.ExitHost == desc.ExitHost &&
			(!hasPreservedGeo || existing.RegisteredAt.After(preservedGeoRegisteredAt)) {
			preservedGeo = existing.GeoLocation
			preservedGeoRegisteredAt = existing.RegisteredAt
			hasPreservedGeo = true
		}
	}
	if hasPreservedGeo {
		desc.GeoLocation = preservedGeo
	}
	for _, replacedID := range replacedIDs {
		delete(s.relays, replacedID)
	}
	// A stable identity re-registering from a new endpoint abandons its old
	// row; the map insert replaces it by key. (Tunnel relays legitimately hit
	// this on most reconnects, because the hub round-robins their public port,
	// so it is normal traffic and not logged.)
	s.relays[id] = desc

	return cloneRelayDescriptor(desc), nil
}

func (s *Store) Heartbeat(id, leaseToken, maxClass string, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.relays[id]
	if !ok {
		return relay.Descriptor{}, ErrRelayNotFound
	}
	if desc.NodeClass == relay.NodeClassFoundation && maxClass != relay.NodeClassFoundation {
		return relay.Descriptor{}, ErrNodeClassForbidden
	}
	// A stable relay ID names an identity, not one registration. Require the
	// unlisted token minted for this exact incarnation so a stale session (or a
	// one-time replay that moved the endpoint) cannot renew whichever endpoint
	// currently occupies the ID. Empty stored tokens are migrated/pre-token
	// stable rows and deliberately fail closed, forcing a fresh registration.
	if desc.IdentityPublicKey != "" && (desc.LeaseToken == "" || subtle.ConstantTimeCompare([]byte(desc.LeaseToken), []byte(leaseToken)) != 1) {
		return relay.Descriptor{}, ErrRelayNotFound
	}

	desc.LastHeartbeatAt = now
	desc.ExpiresAt = now.Add(ttl)
	s.relays[id] = desc

	return cloneRelayDescriptor(desc), nil
}

func (s *Store) UpdateGeo(id, leaseToken string, geo relay.GeoLocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.relays[id]
	if !ok {
		return ErrRelayNotFound
	}
	// GeoIP resolution happens after registration/heartbeat and may finish
	// after another registration has reused this stable ID. Scope the delayed
	// write to the registration that requested it so stale endpoint geography
	// cannot contaminate the replacement descriptor.
	if desc.IdentityPublicKey != "" && (desc.LeaseToken == "" || subtle.ConstantTimeCompare([]byte(desc.LeaseToken), []byte(leaseToken)) != 1) {
		return ErrRelayNotFound
	}
	desc.GeoLocation = geo
	s.relays[id] = desc
	return nil
}

func (s *Store) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	relays, _, err := s.ListRanked(now, limit)
	return relays, err
}

func (s *Store) ListRanked(now time.Time, limit int) ([]relay.Descriptor, map[string]float64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	relays := make([]relay.Descriptor, 0, len(s.relays))
	for _, desc := range s.relays {
		if desc.ExpiresAt.After(now) {
			relays = append(relays, cloneRelayDescriptor(desc))
		}
	}

	snapshots, weights := s.metricSnapshotsLocked(now)
	sortRelayCandidates(relays, snapshots, weights, s.rankingMode)

	if limit > 0 && len(relays) > limit {
		return relays[:limit], weights, nil
	}

	return relays, weights, nil
}

func (s *Store) RelayByID(id string, now time.Time) (relay.Descriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	desc, ok := s.relays[id]
	if !ok || !desc.ExpiresAt.After(now) {
		return relay.Descriptor{}, ErrRelayNotFound
	}
	return cloneRelayDescriptor(desc), nil
}

func cloneRelayDescriptor(desc relay.Descriptor) relay.Descriptor {
	desc.WSSFronts = append([]relay.WSSFrontDescriptor(nil), desc.WSSFronts...)
	return desc
}

func (s *Store) RelayNodeClasses(_ context.Context, ids []string, now time.Time) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	classes := make(map[string]string, len(ids))
	for _, id := range ids {
		desc, ok := s.relays[id]
		if ok && desc.ExpiresAt.After(now) {
			classes[id] = desc.NodeClass
		}
	}
	return classes, nil
}

func (s *Store) Stats(now time.Time) (StoreStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var stats StoreStats
	for _, desc := range s.relays {
		if !desc.ExpiresAt.After(now) {
			continue
		}
		stats.ActiveRelays++
		stats.AdvertisedSessionCapacity += desc.MaxSessions
	}
	return stats, nil
}

// Prune drops expired descriptors and out-of-window metrics. Ranking weights
// are deliberately left alone: they are operator intent about a relay
// identity, not lease state, and must still apply when the relay returns.
func (s *Store) Prune(now time.Time) ([]relay.Descriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []relay.Descriptor
	for id, desc := range s.relays {
		if !desc.ExpiresAt.After(now) {
			expired = append(expired, desc)
			delete(s.relays, id)
		}
	}
	s.pruneMetricsLocked(now.Add(-rankingWindow))
	return expired, nil
}

func (s *Store) RecordRelayTelemetry(_ context.Context, records []TelemetryRecord, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, record := range records {
		s.recordRelayTelemetryLocked(record, now)
	}
	s.pruneMetricsLocked(now.Add(-rankingWindow))
	return nil
}

func (s *Store) RelayRankingWeights(context.Context) (map[string]float64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rankingWeightsLocked(), nil
}

func (s *Store) SetRelayRankingWeight(_ context.Context, id string, weight float64) (float64, error) {
	if err := validateRankingWeight(weight); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := rankingWeightFor(s.rankingWeights, id)
	s.rankingWeights[id] = weight
	return previous, nil
}

func (s *Store) DeleteRelayRankingWeight(_ context.Context, id string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := rankingWeightFor(s.rankingWeights, id)
	delete(s.rankingWeights, id)
	return previous, nil
}

// rankingWeightsLocked copies the override map so callers (and the sort that
// runs after List releases nothing) never alias store state.
func (s *Store) rankingWeightsLocked() map[string]float64 {
	weights := make(map[string]float64, len(s.rankingWeights))
	for id, weight := range s.rankingWeights {
		weights[id] = weight
	}
	return weights
}

func (s *Store) Ping(context.Context) error {
	return nil
}

func (s *Store) Close() error {
	return nil
}

func (s *Store) recordRelayTelemetryLocked(record TelemetryRecord, now time.Time) {
	event := record.Event
	observedAt := record.ReceivedAt
	if observedAt.IsZero() {
		observedAt = event.OccurredAt
	}
	if observedAt.IsZero() {
		observedAt = now
	}

	switch event.Event {
	case "session_heartbeat":
		if event.SessionID == "" || event.RelayID == "" {
			return
		}
		existing := s.sessions[event.SessionID]
		terminalAt := time.Time{}
		if !existing.TerminalAt.IsZero() && !existing.TerminalAt.Before(observedAt) {
			terminalAt = existing.TerminalAt
		}
		s.sessions[event.SessionID] = relaySessionState{
			ClientID:          event.ClientID,
			RelayID:           event.RelayID,
			LastHeartbeatAt:   observedAt,
			TerminalAt:        terminalAt,
			LastMetricEventAt: observedAt,
		}
	case "connection_succeeded", "relay_failover":
		if event.RelayID == "" {
			return
		}
		s.observations = append(s.observations, relayMetricObservation{
			ObservedAt:        observedAt,
			RelayID:           event.RelayID,
			Success:           true,
			TCPMs:             event.Measurements["relay_tcp_ms"],
			TunnelStartMs:     event.Measurements["tunnel_start_ms"],
			InternetProbeMs:   event.Measurements["internet_probe_ms"],
			DownloadMbpsMilli: 0,
		})
	case "relay_attempt_failed":
		if event.RelayID == "" {
			return
		}
		s.observations = append(s.observations, relayMetricObservation{
			ObservedAt: observedAt,
			RelayID:    event.RelayID,
			Failure:    true,
		})
	case "speed_test_completed":
		if event.RelayID == "" {
			return
		}
		s.observations = append(s.observations, relayMetricObservation{
			ObservedAt:        observedAt,
			RelayID:           event.RelayID,
			TTFBMs:            event.Measurements["time_to_first_byte_ms"],
			DownloadMbpsMilli: event.Measurements["download_mbps_milli"],
			IncludesSpeedTest: true,
		})
	case "connection_ended", "tunnel_stopped", "connection_failed":
		if event.SessionID == "" {
			return
		}
		session := s.sessions[event.SessionID]
		if session.RelayID == "" && event.RelayID != "" {
			session.RelayID = event.RelayID
		}
		session.ClientID = firstNonEmpty(session.ClientID, event.ClientID)
		session.TerminalAt = observedAt
		session.LastMetricEventAt = observedAt
		s.sessions[event.SessionID] = session
	}
}

// metricSnapshotsLocked builds the ranking inputs: the 30-minute telemetry
// snapshot per relay and, alongside it, the operator ranking-weight overrides.
// The weights travel as a parallel map rather than a snapshot field because a
// relay with no telemetry has no snapshot entry at all, and its zero value
// must not read as weight 0.
func (s *Store) metricSnapshotsLocked(now time.Time) (map[string]RelayMetricsSnapshot, map[string]float64) {
	snapshots := make(map[string]RelayMetricsSnapshot)
	activeAfter := now.Add(-activeSessionTimeout)
	for _, session := range s.sessions {
		if session.RelayID == "" || !session.TerminalAt.IsZero() || !session.LastHeartbeatAt.After(activeAfter) {
			continue
		}
		snapshot := snapshots[session.RelayID]
		snapshot.ActiveSessions++
		snapshots[session.RelayID] = snapshot
	}

	windowStart := now.Add(-rankingWindow)
	for _, observation := range s.observations {
		if observation.ObservedAt.Before(windowStart) || observation.ObservedAt.After(now) {
			continue
		}
		snapshot := snapshots[observation.RelayID]
		if observation.Success {
			snapshot.Successes++
		}
		if observation.Failure {
			snapshot.Failures++
		}
		addMetricValue(&snapshot.TCPMS, observation.TCPMs)
		addMetricValue(&snapshot.TunnelStartMS, observation.TunnelStartMs)
		addMetricValue(&snapshot.InternetProbeMS, observation.InternetProbeMs)
		addMetricValue(&snapshot.TTFBMS, observation.TTFBMs)
		if observation.IncludesSpeedTest && observation.DownloadMbpsMilli > 0 {
			snapshot.SpeedTests++
			snapshot.DownloadMbpsTotal += float64(observation.DownloadMbpsMilli) / 1000
		}
		snapshots[observation.RelayID] = snapshot
	}
	return snapshots, s.rankingWeightsLocked()
}

func (s *Store) pruneMetricsLocked(cutoff time.Time) {
	kept := s.observations[:0]
	for _, observation := range s.observations {
		if !observation.ObservedAt.Before(cutoff) {
			kept = append(kept, observation)
		}
	}
	s.observations = kept

	for sessionID, session := range s.sessions {
		if !session.TerminalAt.IsZero() && session.TerminalAt.Before(cutoff) {
			delete(s.sessions, sessionID)
		}
	}
}

// normalizeTransport defaults an empty transport to direct so every stored
// descriptor carries a concrete value.
func normalizeTransport(transport string) string {
	if transport == "" {
		return relay.TransportDirect
	}
	return transport
}

// normalizeNodeClass defaults an empty node class to the volunteer class so every
// stored descriptor carries a concrete value. Authorization of a foundation
// claim happens in the handler; the store trusts its caller.
func normalizeNodeClass(class string) string {
	if class == "" {
		return relay.NodeClassVolunteer
	}
	return class
}

func newRelayID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "relay_" + hex.EncodeToString(b[:]), nil
}

func newRelayLeaseToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "lease_" + hex.EncodeToString(b[:]), nil
}
