import XCTest
@testable import OpenRungKit

final class ExitNodeDirectoryTests: XCTestCase {
    func testGroupsRelaysByCountryAndPlacesAtCentroid() async throws {
        let relays = [
            Self.relay(id: "a", publicHost: "1.1.1.1"),
            Self.relay(id: "b", publicHost: "2.2.2.2"),
            Self.relay(id: "c", publicHost: "3.3.3.3"),
        ]
        let geoByHost = [
            "1.1.1.1": Self.geo(countryCode: "JP"),
            "2.2.2.2": Self.geo(countryCode: "JP"),
            "3.3.3.3": Self.geo(countryCode: "US"),
        ]

        let regions = try await ExitNodeDirectory(
            fetchRelays: { Self.response(relays: relays) },
            lookupGeo: { host in geoByHost[host] }
        ).load()

        XCTAssertEqual(regions.count, 2)
        let japan = try XCTUnwrap(regions.first)
        XCTAssertEqual(japan.countryCode, "JP")
        XCTAssertEqual(japan.countryName, "Japan")
        XCTAssertEqual(japan.nodeCount, 2)
        XCTAssertEqual(japan.latitude, 36.20, accuracy: 0.001)
        XCTAssertEqual(japan.longitude, 138.25, accuracy: 0.001)
    }

    func testFallsBackToGeoCoordinatesForUnknownCountry() async throws {
        let regions = try await ExitNodeDirectory(
            fetchRelays: { Self.response(relays: [Self.relay(publicHost: "9.9.9.9")]) },
            lookupGeo: { _ in Self.geo(countryCode: "ZZ", country: "Nowhere", latitude: 12.5, longitude: 34.0) }
        ).load()

        XCTAssertEqual(regions.count, 1)
        XCTAssertEqual(regions[0].countryName, "Nowhere")
        XCTAssertEqual(regions[0].latitude, 12.5, accuracy: 0.001)
        XCTAssertEqual(regions[0].longitude, 34.0, accuracy: 0.001)
    }

    func testSkipsRelaysWithMissingOrBlankGeo() async throws {
        let regions = try await ExitNodeDirectory(
            fetchRelays: {
                Self.response(
                    relays: [
                        Self.relay(id: "a", publicHost: "5.5.5.5"),
                        Self.relay(id: "b", publicHost: "6.6.6.6"),
                    ]
                )
            },
            lookupGeo: { host in host == "5.5.5.5" ? Self.geo(countryCode: "", country: "") : nil }
        ).load()

        XCTAssertTrue(regions.isEmpty)
    }

    private static func geo(
        countryCode: String,
        country: String? = nil,
        latitude: Double = 0.0,
        longitude: Double = 0.0
    ) -> ClientGeoInfo {
        ClientGeoInfo(
            ip: "203.0.113.1",
            country: country ?? countryCode,
            countryCode: countryCode,
            city: "",
            asn: "",
            isp: "",
            organization: "",
            latitude: latitude,
            longitude: longitude
        )
    }

    private static func response(relays: [RelayDescriptor]) -> RelayListResponse {
        RelayListResponse(count: relays.count, serverTime: Date(timeIntervalSince1970: 1_800_000_000), relays: relays)
    }

    private static func relay(
        id: String = "relay-1",
        publicHost: String = "volunteer.example.com",
        relayProtocol: String = RelayConstants.protocolVLESSRealityVision,
        flow: String = RelayConstants.flowVision,
        exitMode: String = RelayConstants.exitModeDirect,
        expiresAt: Date = Date(timeIntervalSince1970: 1_800_000_060)
    ) -> RelayDescriptor {
        RelayDescriptor(
            id: id,
            publicHost: publicHost,
            publicPort: 443,
            relayProtocol: relayProtocol,
            clientID: "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
            realityPublicKey: "reality-public-key",
            shortID: "5f7a8d9c01ab23cd",
            serverName: "www.cloudflare.com",
            flow: flow,
            exitMode: exitMode,
            maxSessions: 8,
            maxMbps: 20,
            volunteerVersion: "dev",
            registeredAt: Date(timeIntervalSince1970: 1_800_000_000),
            lastHeartbeatAt: Date(timeIntervalSince1970: 1_800_000_000),
            expiresAt: expiresAt
        )
    }
}
