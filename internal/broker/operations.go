package broker

import "time"

const activeSessionTimeout = 150 * time.Second

type OperationalTelemetryStats struct {
	ClientsSeen     int
	SessionsStarted int
	Failures        int
	ActiveClients   int
	ActiveSessions  int
}

func BuildOperationalTelemetryStats(records []TelemetryRecord, now time.Time, window time.Duration) OperationalTelemetryStats {
	start := now.Add(-window)
	clients := make(map[string]struct{})
	sessionsStarted := make(map[string]struct{})
	failures := make(map[string]struct{})
	type activity struct {
		clientID      string
		lastHeartbeat time.Time
		terminal      bool
	}
	activityBySession := make(map[string]*activity)
	stats := OperationalTelemetryStats{}

	for _, record := range records {
		event := record.Event
		if event.OccurredAt.Before(start) || event.OccurredAt.After(now) {
			continue
		}
		if event.ClientID != "" {
			clients[event.ClientID] = struct{}{}
		}
		sessionActivity := activityBySession[event.SessionID]
		if sessionActivity == nil {
			sessionActivity = &activity{clientID: event.ClientID}
			activityBySession[event.SessionID] = sessionActivity
		}
		switch event.Event {
		case "connection_attempted":
			if event.SessionID != "" {
				sessionsStarted[event.SessionID] = struct{}{}
			}
		case "connection_failed":
			sessionActivity.terminal = true
			key := event.EventID
			if key == "" {
				key = event.SessionID
			}
			failures[key] = struct{}{}
		case "connection_ended", "tunnel_stopped":
			sessionActivity.terminal = true
		case "session_heartbeat":
			if record.ReceivedAt.After(sessionActivity.lastHeartbeat) {
				sessionActivity.lastHeartbeat = record.ReceivedAt
			}
		}
	}

	stats.ClientsSeen = len(clients)
	stats.SessionsStarted = len(sessionsStarted)
	stats.Failures = len(failures)
	activeClients := make(map[string]struct{})
	for _, activity := range activityBySession {
		if !activity.terminal && activity.lastHeartbeat.After(now.Add(-activeSessionTimeout)) {
			stats.ActiveSessions++
			activeClients[activity.clientID] = struct{}{}
		}
	}
	stats.ActiveClients = len(activeClients)
	return stats
}
