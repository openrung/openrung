package broker

import "time"

// TelemetryQuerier is the dashboard's read path, mirroring its two admin API
// endpoints. The in-memory implementation aggregates a TelemetryReader's
// records in Go; the Postgres telemetry store aggregates in SQL so dashboard
// cost does not scale with event volume.
type TelemetryQuerier interface {
	TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error)
	// TelemetrySessions returns one page of the window's sessions, newest
	// last-seen first, plus the total session count in the window.
	TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error)
}

// telemetryAppCounter is the optional capability a TelemetryReader exposes
// when it keeps an hourly application-connection rollup (the JSONL sink does);
// the dashboard's top-apps panel is fed from it, never from stored records.
type telemetryAppCounter interface {
	AppConnectionCounts(now time.Time, window time.Duration) map[string]int
}

// telemetryReaderQuerier adapts any TelemetryReader (the JSONL sink's bounded
// in-memory record set) to the dashboard's query interface by aggregating in
// Go on every request.
type telemetryReaderQuerier struct {
	reader TelemetryReader
	apps   telemetryAppCounter
}

func newTelemetryReaderQuerier(reader TelemetryReader) telemetryReaderQuerier {
	querier := telemetryReaderQuerier{reader: reader}
	if counter, ok := reader.(telemetryAppCounter); ok {
		querier.apps = counter
	}
	return querier
}

func (q telemetryReaderQuerier) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	var appCounts map[string]int
	if q.apps != nil {
		appCounts = q.apps.AppConnectionCounts(now, window)
	}
	return buildTelemetryOverview(q.reader.TelemetryRecords(now.Add(-window)), appCounts, now, window), nil
}

func (q telemetryReaderQuerier) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	overview := buildTelemetryOverview(q.reader.TelemetryRecords(now.Add(-window)), nil, now, window)
	total := len(overview.Recent)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	return append([]sessionSummary{}, overview.Recent[offset:end]...), total, nil
}
