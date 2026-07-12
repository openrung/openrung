package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"openrung/internal/relay"
)

var ErrRelayNotFound = errors.New("relay not found")

// ErrNodeClassForbidden is returned by Heartbeat when the presented credential
// cannot vouch for the relay's stored node class. Refusing the lease extension
// (rather than downgrading the row) means an unauthorized foundation label
// expires within one lease TTL, while an attacker who heartbeats a foundation
// relay's public ID with weaker credentials cannot damage the row.
var ErrNodeClassForbidden = errors.New("credential cannot heartbeat a relay of this node class")

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
	Heartbeat(id, maxClass string, now time.Time, ttl time.Duration) (relay.Descriptor, error)
	UpdateGeo(string, relay.GeoLocation) error
	List(time.Time, int) ([]relay.Descriptor, error)
	Stats(time.Time) (StoreStats, error)
	Prune(time.Time) ([]relay.Descriptor, error)
	RecordRelayTelemetry(context.Context, []TelemetryRecord, time.Time) error
	Ping(context.Context) error
	Close() error
}

type Store struct {
	mu           sync.RWMutex
	relays       map[string]relay.Descriptor
	sessions     map[string]relaySessionState
	observations []relayMetricObservation
	rankingMode  RankingMode
}

type StoreStats struct {
	ActiveVolunteers          int
	AdvertisedSessionCapacity int
}

func NewStore() *Store {
	return NewStoreWithRanking(RankingModeGlobal)
}

func NewStoreWithRanking(rankingMode RankingMode) *Store {
	return &Store{
		relays:      make(map[string]relay.Descriptor),
		sessions:    make(map[string]relaySessionState),
		rankingMode: normalizeRankingMode(rankingMode),
	}
}

func (s *Store) Register(req relay.RegisterRequest, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	id, err := newRelayID()
	if err != nil {
		return relay.Descriptor{}, err
	}

	desc := relay.Descriptor{
		ID:               id,
		Label:            req.Label,
		NodeClass:        normalizeNodeClass(req.NodeClass),
		PublicHost:       req.PublicHost,
		PublicPort:       req.PublicPort,
		ExitHost:         req.ExitHost,
		Protocol:         req.Protocol,
		ClientID:         req.ClientID,
		RealityPublicKey: req.RealityPublicKey,
		ShortID:          req.ShortID,
		ServerName:       req.ServerName,
		Flow:             req.Flow,
		ExitMode:         req.ExitMode,
		MaxSessions:      req.MaxSessions,
		MaxMbps:          req.MaxMbps,
		VolunteerVersion: req.VolunteerVersion,
		Transport:        normalizeTransport(req.Transport),
		PunchCapable:     req.PunchCapable,
		PunchEndpoint:    req.PunchEndpoint,
		RegisteredAt:     now,
		LastHeartbeatAt:  now,
		ExpiresAt:        now.Add(ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.relays[id] = desc

	return desc, nil
}

func (s *Store) Heartbeat(id, maxClass string, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.relays[id]
	if !ok {
		return relay.Descriptor{}, ErrRelayNotFound
	}
	if desc.NodeClass == relay.NodeClassFoundation && maxClass != relay.NodeClassFoundation {
		return relay.Descriptor{}, ErrNodeClassForbidden
	}

	desc.LastHeartbeatAt = now
	desc.ExpiresAt = now.Add(ttl)
	s.relays[id] = desc

	return desc, nil
}

func (s *Store) UpdateGeo(id string, geo relay.GeoLocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.relays[id]
	if !ok {
		return ErrRelayNotFound
	}
	desc.GeoLocation = geo
	s.relays[id] = desc
	return nil
}

func (s *Store) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	relays := make([]relay.Descriptor, 0, len(s.relays))
	for _, desc := range s.relays {
		if desc.ExpiresAt.After(now) {
			relays = append(relays, desc)
		}
	}

	sortRelayCandidates(relays, s.metricSnapshotsLocked(now), s.rankingMode)

	if limit > 0 && len(relays) > limit {
		return relays[:limit], nil
	}

	return relays, nil
}

func (s *Store) Stats(now time.Time) (StoreStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var stats StoreStats
	for _, desc := range s.relays {
		if !desc.ExpiresAt.After(now) {
			continue
		}
		stats.ActiveVolunteers++
		stats.AdvertisedSessionCapacity += desc.MaxSessions
	}
	return stats, nil
}

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

func (s *Store) metricSnapshotsLocked(now time.Time) map[string]RelayMetricsSnapshot {
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
	return snapshots
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

// normalizeNodeClass defaults an empty node class to volunteer so every
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
