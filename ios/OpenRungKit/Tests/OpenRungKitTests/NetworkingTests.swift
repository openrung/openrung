import XCTest
@testable import OpenRungKit

final class NetworkingTests: XCTestCase {
    private let broker = URL(string: "http://54.238.185.205:8080/")!

    func testRelaysURLPreservesPortAndAppendsLimit() throws {
        let url = try BrokerClient.relaysURL(brokerURL: broker, limit: 5)
        XCTAssertEqual(url.absoluteString, "http://54.238.185.205:8080/api/v1/relays?limit=5")
    }

    func testRelaysURLClampsNonPositiveLimit() throws {
        let url = try BrokerClient.relaysURL(brokerURL: broker, limit: 0)
        XCTAssertEqual(url.absoluteString, "http://54.238.185.205:8080/api/v1/relays?limit=1")
    }

    func testCandidatesPutPrimaryFirstThenFallbacks() {
        let primary = URL(string: "https://primary.example/")!
        let fallbacks = [URL(string: "http://fallback-a/")!, URL(string: "http://fallback-b/")!]
        XCTAssertEqual(
            BrokerClient.candidates(primary: primary, fallbacks: fallbacks),
            [primary] + fallbacks
        )
    }

    func testCandidatesDeduplicatePrimaryThatIsAlsoAFallback() {
        let shared = URL(string: "http://fallback-a/")!
        let other = URL(string: "http://fallback-b/")!
        XCTAssertEqual(
            BrokerClient.candidates(primary: shared, fallbacks: [shared, other]),
            [shared, other]
        )
    }

    func testCandidatesIgnoreNilPrimary() {
        let fallbacks = [URL(string: "http://fallback-a/")!]
        XCTAssertEqual(BrokerClient.candidates(primary: nil, fallbacks: fallbacks), fallbacks)
    }

    func testCandidatesKeepDefaultOrderWhenPrimaryEchoesANonFirstDefault() {
        // Migration guard: a persisted primary that echoes the raw-IP default must not reorder the
        // HTTPS-first defaults.
        let cf = URL(string: "https://broker.example/")!
        let ip = URL(string: "http://203.0.113.10:8080/")!
        XCTAssertEqual(BrokerClient.candidates(primary: ip, fallbacks: [cf, ip]), [cf, ip])
    }

    func testSpeedTestURLAppendsPath() throws {
        let url = try SpeedTestClient.speedTestURL(brokerURL: broker)
        XCTAssertEqual(url.absoluteString, "http://54.238.185.205:8080/api/v1/speed-test")
    }

    func testSpeedTestURLPreservesBasePath() throws {
        let url = try SpeedTestClient.speedTestURL(brokerURL: URL(string: "https://example.com/broker/")!)
        XCTAssertEqual(url.absoluteString, "https://example.com/broker/api/v1/speed-test")
    }

    func testTelemetryURLAppendsPath() throws {
        let url = try TelemetryClient.telemetryURL(brokerURL: broker)
        XCTAssertEqual(url.absoluteString, "http://54.238.185.205:8080/api/v1/telemetry/events")
    }

    func testCalculateMbps() {
        // 10,000,000 bytes in 1.0s == 80 Mbps
        XCTAssertEqual(SpeedTestClient.calculateMbps(bytes: 10_000_000, durationNs: 1_000_000_000), 80, accuracy: 0.0001)
    }

    func testInternetProbeAcceptsStatus() {
        XCTAssertTrue(InternetProbe.acceptsHTTPStatus(204))
        XCTAssertTrue(InternetProbe.acceptsHTTPStatus(200))
        XCTAssertFalse(InternetProbe.acceptsHTTPStatus(302))
        XCTAssertFalse(InternetProbe.acceptsHTTPStatus(500))
    }

    func testGeoDecodeProducesLabelAndAttributes() throws {
        let json = Data("""
        {"ip":"203.0.113.5","success":true,"country":"United States","country_code":"US","city":"Austin","latitude":30.27,"longitude":-97.74,"connection":{"asn":15169,"org":"Google LLC","isp":"Google"}}
        """.utf8)
        let info = try GeoIpClient.decode(json)
        XCTAssertEqual(info.city, "Austin")
        XCTAssertEqual(info.country, "United States")
        XCTAssertEqual(info.asn, "AS15169")
        XCTAssertEqual(info.latitude, 30.27, accuracy: 0.001)
        XCTAssertEqual(info.longitude, -97.74, accuracy: 0.001)
        XCTAssertEqual(info.locationLabel(), "Austin, United States")
        XCTAssertEqual(info.telemetryAttributes()["country_code"], "US")
        XCTAssertEqual(info.telemetryAttributes()["isp"], "Google")
    }

    func testGeoDecodeRejectsUnsuccessfulResponse() {
        let json = Data(#"{"success":false,"message":"rate limited"}"#.utf8)
        XCTAssertThrowsError(try GeoIpClient.decode(json))
    }

    func testTelemetryEventEncodesSnakeCaseKeys() throws {
        let event = TelemetryEvent(
            eventId: "e1",
            event: "connection_succeeded",
            occurredAt: "2026-01-01T00:00:00Z",
            clientId: "c1",
            sessionId: "s1",
            relayId: "r1",
            measurements: ["relay_tcp_ms": 12]
        )
        let json = String(decoding: try JSONEncoder().encode(event), as: UTF8.self)
        XCTAssertTrue(json.contains("\"schema_version\":1"))
        XCTAssertTrue(json.contains("\"event_id\":\"e1\""))
        XCTAssertTrue(json.contains("\"client_id\":\"c1\""))
        XCTAssertTrue(json.contains("\"session_id\":\"s1\""))
        XCTAssertTrue(json.contains("\"relay_id\":\"r1\""))
        XCTAssertTrue(json.contains("\"relay_tcp_ms\":12"))
        // Optional fields left nil must be omitted, not encoded as null.
        XCTAssertFalse(json.contains("application_package"))
        XCTAssertFalse(json.contains("\"protocol\""))
    }

    func testTelemetryBatchRoundTrips() throws {
        let event = TelemetryEvent(eventId: "e1", event: "session_heartbeat", occurredAt: "t", clientId: "c", sessionId: "s")
        let data = try JSONEncoder().encode(TelemetryBatch(events: [event]))
        let decoded = try JSONDecoder().decode(TelemetryBatch.self, from: data)
        XCTAssertEqual(decoded.events.first?.eventId, "e1")
        XCTAssertEqual(decoded.events.first?.schemaVersion, 1)
    }
}
