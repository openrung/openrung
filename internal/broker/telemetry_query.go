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

// telemetryReaderQuerier adapts any TelemetryReader (the JSONL sink's bounded
// in-memory record set) to the dashboard's query interface by aggregating in
// Go on every request.
type telemetryReaderQuerier struct {
	reader TelemetryReader
}

func newTelemetryReaderQuerier(reader TelemetryReader) telemetryReaderQuerier {
	return telemetryReaderQuerier{reader: reader}
}

func (q telemetryReaderQuerier) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	return buildTelemetryOverview(q.reader.TelemetryRecords(now.Add(-window)), now, window), nil
}

func (q telemetryReaderQuerier) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	overview := buildTelemetryOverview(q.reader.TelemetryRecords(now.Add(-window)), now, window)
	total := len(overview.Recent)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	return append([]sessionSummary{}, overview.Recent[offset:end]...), total, nil
}
