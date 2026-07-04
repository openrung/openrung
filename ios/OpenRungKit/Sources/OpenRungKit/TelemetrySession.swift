import Foundation

/// Monotonic millisecond clock (excludes time the device is asleep), the iOS analog of
/// Android's `SystemClock.elapsedRealtime()` used for session durations.
public enum MonotonicClock {
    public static func nowMs() -> Int64 {
        Int64(ProcessInfo.processInfo.systemUptime * 1_000)
    }
}

/// An active telemetry session. Port of Android `TelemetryManager.Session`. Codable so it can be
/// shared between the extension (writer) and the app (reader) via app-group storage.
public struct TelemetrySession: Codable, Sendable, Equatable {
    public let id: String
    public let clientId: String
    public let brokerURL: String
    public let startedElapsedMs: Int64
    public var relayId: String?
    public var connectedElapsedMs: Int64?
    public var geoAttributes: [String: String]

    public init(
        id: String,
        clientId: String,
        brokerURL: String,
        startedElapsedMs: Int64,
        relayId: String? = nil,
        connectedElapsedMs: Int64? = nil,
        geoAttributes: [String: String] = [:]
    ) {
        self.id = id
        self.clientId = clientId
        self.brokerURL = brokerURL
        self.startedElapsedMs = startedElapsedMs
        self.relayId = relayId
        self.connectedElapsedMs = connectedElapsedMs
        self.geoAttributes = geoAttributes
    }
}

/// Builds a `session_heartbeat` event, or nil if the session is not yet connected.
/// Port of Android `buildSessionHeartbeat`.
public func buildSessionHeartbeat(
    session: TelemetrySession,
    occurredAt: String,
    elapsedRealtimeMs: Int64,
    attributes: [String: String]
) -> TelemetryEvent? {
    guard let relayId = session.relayId, let connectedElapsedMs = session.connectedElapsedMs else {
        return nil
    }
    var merged = attributes
    merged["connection_state"] = "connected"
    return TelemetryEvent(
        eventId: UUID().uuidString,
        event: "session_heartbeat",
        occurredAt: occurredAt,
        clientId: session.clientId,
        sessionId: session.id,
        relayId: relayId,
        attributes: merged,
        measurements: [
            "session_duration_ms": max(elapsedRealtimeMs - session.startedElapsedMs, 0),
            "connected_duration_ms": max(elapsedRealtimeMs - connectedElapsedMs, 0),
        ]
    )
}
