package broker

import (
	"testing"
	"time"
)

func TestBuildOperationalTelemetryStats(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	records := []TelemetryRecord{
		operationalRecord(now.Add(-4*time.Minute), "client-1", "session-1", "connection_attempted"),
		operationalRecord(now.Add(-3*time.Minute), "client-1", "session-1", "connection_attempted"),
		operationalRecord(now.Add(-2*time.Minute), "client-2", "session-2", "connection_failed"),
		operationalRecord(now.Add(-6*time.Minute), "old-client", "old-session", "connection_attempted"),
		operationalRecord(now.Add(time.Minute), "future-client", "future-session", "connection_failed"),
		operationalRecord(now.Add(-time.Minute), "client-3", "active-session", "session_heartbeat"),
		operationalRecord(now.Add(-time.Minute), "client-4", "ended-session", "session_heartbeat"),
		operationalRecord(now.Add(-30*time.Second), "client-4", "ended-session", "connection_ended"),
	}

	stats := BuildOperationalTelemetryStats(records, now, 5*time.Minute)
	if stats.ClientsSeen != 4 || stats.SessionsStarted != 1 || stats.Failures != 1 || stats.ActiveClients != 1 || stats.ActiveSessions != 1 {
		t.Fatalf("unexpected operational telemetry stats: %+v", stats)
	}
}

func operationalRecord(at time.Time, clientID, sessionID, eventName string) TelemetryRecord {
	return TelemetryRecord{ReceivedAt: at, Event: TelemetryEvent{
		SchemaVersion: 1,
		EventID:       eventName + "-" + sessionID,
		Event:         eventName,
		OccurredAt:    at,
		ClientID:      clientID,
		SessionID:     sessionID,
	}}
}
