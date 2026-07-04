import Foundation

/// Pure queue operations for the telemetry outbox. Mirrors Android `TelemetryManager`'s
/// enqueue cap (500) and id-based removal. The persistence/coordination layer lives in the
/// app/extension `Shared/TelemetryOutbox`.
public enum TelemetryOutboxState {
    public static let maxQueued = 500
    public static let uploadBatchSize = 200

    public static func appended(_ events: [TelemetryEvent], _ event: TelemetryEvent, max: Int = maxQueued) -> [TelemetryEvent] {
        Array((events + [event]).suffix(max))
    }

    public static func removing(_ events: [TelemetryEvent], ids: Set<String>) -> [TelemetryEvent] {
        events.filter { ids.contains($0.eventId) == false }
    }

    public static func applyingGeoAttributes(_ events: [TelemetryEvent], _ attributes: [String: String], toSessionId sessionId: String) -> [TelemetryEvent] {
        events.map { event in
            guard event.sessionId == sessionId else { return event }
            var copy = event
            copy.attributes.merge(attributes) { _, new in new }
            return copy
        }
    }
}
