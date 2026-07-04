import Foundation
import OpenRungKit

/// Façade combining session, outbox, device attributes, and the telemetry HTTP client.
/// Usable from both processes — the extension drives the connection lifecycle/heartbeat, the app
/// records speed-test results. Port of Android `TelemetryManager`.
enum TelemetryManager {
    static func clientId() -> String {
        ClientIdentity.getOrCreate()
    }

    @discardableResult
    static func beginSession(brokerURL: String) -> TelemetrySession {
        let session = TelemetrySession(
            id: UUID().uuidString,
            clientId: clientId(),
            brokerURL: brokerURL,
            startedElapsedMs: MonotonicClock.nowMs()
        )
        TelemetrySessionStore.save(session)
        return session
    }

    static func activeSession() -> TelemetrySession? {
        TelemetrySessionStore.current()
    }

    static func markConnected(relayId: String) {
        guard var session = TelemetrySessionStore.current() else { return }
        session.relayId = relayId
        session.connectedElapsedMs = MonotonicClock.nowMs()
        TelemetrySessionStore.save(session)
    }

    static func setGeoInfo(_ geo: ClientGeoInfo) {
        guard var session = TelemetrySessionStore.current() else { return }
        let attributes = geo.telemetryAttributes()
        session.geoAttributes = attributes
        TelemetrySessionStore.save(session)
        TelemetryOutbox.applyGeoAttributes(attributes, toSessionId: session.id)
        record("client_geo_resolved")
    }

    static func record(
        _ event: String,
        relayId: String? = nil,
        attributes: [String: String] = [:],
        measurements: [String: Int64] = [:]
    ) {
        guard let session = TelemetrySessionStore.current() else { return }
        var merged = DeviceAttributes.current()
        merged.merge(session.geoAttributes) { _, new in new }
        merged.merge(attributes) { _, new in new }
        TelemetryOutbox.enqueue(
            TelemetryEvent(
                eventId: UUID().uuidString,
                event: event,
                occurredAt: iso8601Now(),
                clientId: session.clientId,
                sessionId: session.id,
                relayId: relayId ?? session.relayId,
                attributes: merged,
                measurements: measurements
            )
        )
    }

    static func endSession(reason: String) {
        guard let session = TelemetrySessionStore.current() else { return }
        let now = MonotonicClock.nowMs()
        var measurements: [String: Int64] = ["session_duration_ms": max(now - session.startedElapsedMs, 0)]
        if let connected = session.connectedElapsedMs {
            measurements["connection_duration_ms"] = max(now - connected, 0)
        }
        record("connection_ended", relayId: session.relayId, attributes: ["reason": reason], measurements: measurements)
        TelemetrySessionStore.save(nil)
    }

    static func recordSpeedTest(_ result: SpeedTestResult) {
        record(
            "speed_test_completed",
            attributes: ["provider": "openrung_broker", "test_type": "manual_download"],
            measurements: [
                "bytes_downloaded": result.bytesDownloaded,
                "download_duration_ms": result.durationMs,
                "time_to_first_byte_ms": result.timeToFirstByteMs,
                "download_mbps_milli": Int64(result.downloadMbps * 1_000),
            ]
        )
    }

    /// Sends a heartbeat plus a batch of queued events (mirrors Android's piggyback strategy).
    /// Best-effort: failures leave queued events intact for the next attempt.
    static func sendHeartbeat() async {
        guard
            let session = TelemetrySessionStore.current(),
            let brokerURL = URL(string: session.brokerURL)
        else { return }

        var attributes = DeviceAttributes.current()
        attributes.merge(session.geoAttributes) { _, new in new }
        guard let heartbeat = buildSessionHeartbeat(
            session: session,
            occurredAt: iso8601Now(),
            elapsedRealtimeMs: MonotonicClock.nowMs(),
            attributes: attributes
        ) else { return }

        let queued = TelemetryOutbox.peek(max: TelemetryOutboxState.uploadBatchSize - 1)
        do {
            let client = try TelemetryClient(brokerURL: brokerURL)
            try await client.send(queued + [heartbeat])
            if queued.isEmpty == false {
                TelemetryOutbox.remove(ids: Set(queued.map(\.eventId)))
                try? await flush(brokerURL: session.brokerURL)
            }
        } catch {
            // best-effort
        }
    }

    static func flush(brokerURL: String) async throws {
        guard let url = URL(string: brokerURL) else { return }
        let client = try TelemetryClient(brokerURL: url)
        while true {
            let batch = TelemetryOutbox.peek(max: TelemetryOutboxState.uploadBatchSize)
            if batch.isEmpty { return }
            try await client.send(batch)
            TelemetryOutbox.remove(ids: Set(batch.map(\.eventId)))
        }
    }

    private static func iso8601Now() -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.string(from: Date())
    }
}
