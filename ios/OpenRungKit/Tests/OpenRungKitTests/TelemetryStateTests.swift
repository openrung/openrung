import XCTest
@testable import OpenRungKit

final class TelemetryStateTests: XCTestCase {
    func testConnectionStatusDerivedFlags() {
        XCTAssertTrue(ConnectionStatus.connecting.isWorking)
        XCTAssertTrue(ConnectionStatus.preparing.isWorking)
        XCTAssertTrue(ConnectionStatus.disconnecting.isWorking)
        XCTAssertFalse(ConnectionStatus.connected.isWorking)
        XCTAssertTrue(ConnectionStatus.connected.isConnected)
        XCTAssertEqual(ConnectionStatus.preparing.displayLabel, "Preparing VPN")
    }

    func testActivityLogLineIsTimestamped() {
        let date = Date(timeIntervalSince1970: 0)
        let line = ActivityLog.line("fetching relays", at: date)
        XCTAssertTrue(line.hasPrefix("["))
        XCTAssertTrue(line.contains("] fetching relays"))
    }

    func testActivityLogCapsAtMax() {
        var lines: [String] = []
        for index in 0..<100 {
            lines = ActivityLog.appended(lines, "line \(index)", max: 80)
        }
        XCTAssertEqual(lines.count, 80)
        XCTAssertEqual(lines.first, "line 20")
        XCTAssertEqual(lines.last, "line 99")
    }

    func testOutboxAppendCapsAndKeepsNewest() {
        var events: [TelemetryEvent] = []
        for index in 0..<600 {
            events = TelemetryOutboxState.appended(events, event(id: "e\(index)"), max: 500)
        }
        XCTAssertEqual(events.count, 500)
        XCTAssertEqual(events.first?.eventId, "e100")
        XCTAssertEqual(events.last?.eventId, "e599")
    }

    func testOutboxRemovingByIds() {
        let events = [event(id: "a"), event(id: "b"), event(id: "c")]
        let remaining = TelemetryOutboxState.removing(events, ids: ["a", "c"])
        XCTAssertEqual(remaining.map(\.eventId), ["b"])
    }

    func testOutboxAppliesGeoOnlyToMatchingSession() {
        let events = [event(id: "a", sessionId: "s1"), event(id: "b", sessionId: "s2")]
        let updated = TelemetryOutboxState.applyingGeoAttributes(events, ["city": "Austin"], toSessionId: "s1")
        XCTAssertEqual(updated[0].attributes["city"], "Austin")
        XCTAssertNil(updated[1].attributes["city"])
    }

    func testHeartbeatNilUntilConnected() {
        let session = TelemetrySession(id: "s", clientId: "c", brokerURL: "http://b", startedElapsedMs: 0)
        XCTAssertNil(buildSessionHeartbeat(session: session, occurredAt: "t", elapsedRealtimeMs: 1_000, attributes: [:]))
    }

    func testHeartbeatMeasuresDurations() {
        let session = TelemetrySession(id: "s", clientId: "c", brokerURL: "http://b", startedElapsedMs: 0, relayId: "r", connectedElapsedMs: 500)
        let event = buildSessionHeartbeat(session: session, occurredAt: "t", elapsedRealtimeMs: 2_000, attributes: ["app_version": "1.0"])
        XCTAssertEqual(event?.event, "session_heartbeat")
        XCTAssertEqual(event?.relayId, "r")
        XCTAssertEqual(event?.attributes["connection_state"], "connected")
        XCTAssertEqual(event?.measurements["session_duration_ms"], 2_000)
        XCTAssertEqual(event?.measurements["connected_duration_ms"], 1_500)
    }

    private func event(id: String, sessionId: String = "s1") -> TelemetryEvent {
        TelemetryEvent(eventId: id, event: "test", occurredAt: "t", clientId: "c", sessionId: sessionId)
    }
}
