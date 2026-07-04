import Foundation

/// Load state of the exit-node map directory (the list of available exit-node regions).
public enum DirectoryStatus: Equatable, Sendable {
    case idle
    case loading
    case loaded
    case failed
}

/// One country/region on the exit-node map. Volunteer relays are grouped by the country resolved for
/// their public host, so a single marker may stand for several nodes (`nodeCount`).
public struct ExitNodeRegion: Equatable, Identifiable, Sendable {
    public var id: String { countryCode }

    public let countryCode: String
    public let countryName: String
    public let latitude: Double
    public let longitude: Double
    public let nodeCount: Int

    public init(
        countryCode: String,
        countryName: String,
        latitude: Double,
        longitude: Double,
        nodeCount: Int
    ) {
        self.countryCode = countryCode
        self.countryName = countryName
        self.latitude = latitude
        self.longitude = longitude
        self.nodeCount = nodeCount
    }
}

/// A location the user has previously connected through, shown in the main-screen "Recents" row.
public struct RecentNode: Codable, Equatable, Identifiable, Sendable {
    public var id: String { countryCode }

    public let countryCode: String
    public let label: String
    public let latitude: Double
    public let longitude: Double

    public init(countryCode: String, label: String, latitude: Double, longitude: Double) {
        self.countryCode = countryCode
        self.label = label
        self.latitude = latitude
        self.longitude = longitude
    }
}

/**
 Static country centroid + display-name table used to place exit-node markers on the map without a
 per-host geocoding round trip. Asia-Pacific is covered densely (the volunteer network's focus);
 common VPN-exit countries elsewhere are included so a stray relay still lands somewhere sensible.

 Coordinates are approximate country centroids (latitude, longitude in degrees).
 */
public enum CountryGeo {
    public struct Centroid: Equatable, Sendable {
        public let name: String
        public let latitude: Double
        public let longitude: Double
    }

    private static let table: [String: Centroid] = [
        // East Asia
        "JP": Centroid(name: "Japan", latitude: 36.20, longitude: 138.25),
        "KR": Centroid(name: "South Korea", latitude: 36.50, longitude: 127.85),
        "KP": Centroid(name: "North Korea", latitude: 40.34, longitude: 127.51),
        "CN": Centroid(name: "China", latitude: 35.86, longitude: 104.20),
        "HK": Centroid(name: "Hong Kong", latitude: 22.32, longitude: 114.17),
        "MO": Centroid(name: "Macau", latitude: 22.20, longitude: 113.55),
        "TW": Centroid(name: "Taiwan", latitude: 23.70, longitude: 121.00),
        "MN": Centroid(name: "Mongolia", latitude: 46.86, longitude: 103.85),
        // Southeast Asia
        "SG": Centroid(name: "Singapore", latitude: 1.35, longitude: 103.82),
        "MY": Centroid(name: "Malaysia", latitude: 4.21, longitude: 101.98),
        "ID": Centroid(name: "Indonesia", latitude: -2.50, longitude: 118.00),
        "TH": Centroid(name: "Thailand", latitude: 15.87, longitude: 100.99),
        "VN": Centroid(name: "Vietnam", latitude: 14.06, longitude: 108.28),
        "PH": Centroid(name: "Philippines", latitude: 12.88, longitude: 121.77),
        "KH": Centroid(name: "Cambodia", latitude: 12.57, longitude: 104.99),
        "LA": Centroid(name: "Laos", latitude: 19.86, longitude: 102.50),
        "MM": Centroid(name: "Myanmar", latitude: 21.91, longitude: 95.96),
        "BN": Centroid(name: "Brunei", latitude: 4.54, longitude: 114.73),
        "TL": Centroid(name: "Timor-Leste", latitude: -8.87, longitude: 125.73),
        // South Asia
        "IN": Centroid(name: "India", latitude: 22.00, longitude: 79.00),
        "BD": Centroid(name: "Bangladesh", latitude: 23.68, longitude: 90.36),
        "PK": Centroid(name: "Pakistan", latitude: 30.38, longitude: 69.35),
        "LK": Centroid(name: "Sri Lanka", latitude: 7.87, longitude: 80.77),
        "NP": Centroid(name: "Nepal", latitude: 28.39, longitude: 84.12),
        "BT": Centroid(name: "Bhutan", latitude: 27.51, longitude: 90.43),
        "MV": Centroid(name: "Maldives", latitude: 3.20, longitude: 73.22),
        "AF": Centroid(name: "Afghanistan", latitude: 33.94, longitude: 67.71),
        // Oceania
        "AU": Centroid(name: "Australia", latitude: -25.27, longitude: 133.78),
        "NZ": Centroid(name: "New Zealand", latitude: -41.00, longitude: 174.00),
        "FJ": Centroid(name: "Fiji", latitude: -17.71, longitude: 178.07),
        "PG": Centroid(name: "Papua New Guinea", latitude: -6.31, longitude: 143.96),
        // Central Asia
        "KZ": Centroid(name: "Kazakhstan", latitude: 48.02, longitude: 66.92),
        "UZ": Centroid(name: "Uzbekistan", latitude: 41.38, longitude: 64.59),
        "KG": Centroid(name: "Kyrgyzstan", latitude: 41.20, longitude: 74.77),
        "TJ": Centroid(name: "Tajikistan", latitude: 38.86, longitude: 71.28),
        "TM": Centroid(name: "Turkmenistan", latitude: 38.97, longitude: 59.56),
        // West Asia / Middle East
        "AE": Centroid(name: "United Arab Emirates", latitude: 23.42, longitude: 53.85),
        "SA": Centroid(name: "Saudi Arabia", latitude: 23.89, longitude: 45.08),
        "TR": Centroid(name: "Turkey", latitude: 38.96, longitude: 35.24),
        "IR": Centroid(name: "Iran", latitude: 32.43, longitude: 53.69),
        "IL": Centroid(name: "Israel", latitude: 31.05, longitude: 34.85),
        "QA": Centroid(name: "Qatar", latitude: 25.35, longitude: 51.18),
        // Common exit countries outside APAC
        "RU": Centroid(name: "Russia", latitude: 61.52, longitude: 105.32),
        "US": Centroid(name: "United States", latitude: 39.00, longitude: -98.00),
        "CA": Centroid(name: "Canada", latitude: 56.13, longitude: -106.35),
        "GB": Centroid(name: "United Kingdom", latitude: 55.38, longitude: -3.44),
        "DE": Centroid(name: "Germany", latitude: 51.17, longitude: 10.45),
        "NL": Centroid(name: "Netherlands", latitude: 52.13, longitude: 5.29),
        "FR": Centroid(name: "France", latitude: 46.60, longitude: 2.45),
        "FI": Centroid(name: "Finland", latitude: 61.92, longitude: 25.75),
        "SE": Centroid(name: "Sweden", latitude: 60.13, longitude: 18.64),
    ]

    /// Centroid for an ISO 3166-1 alpha-2 country code (case-insensitive), or nil if unknown.
    public static func centroid(_ countryCode: String) -> Centroid? {
        table[normalizedCountryCode(countryCode)]
    }

    /// Display name for a country code, or nil if unknown.
    public static func displayName(_ countryCode: String) -> String? {
        centroid(countryCode)?.name
    }

    static func normalizedCountryCode(_ countryCode: String) -> String {
        countryCode.trimmingCharacters(in: .whitespacesAndNewlines).uppercased()
    }
}

/**
 Builds the exit-node map directory: fetches the broker's relay list, resolves each usable relay's
 country via GeoIP, and groups them into one `ExitNodeRegion` per country (placed at a curated
 centroid, falling back to the GeoIP coordinate when the country is not in `CountryGeo`).

 Both the relay fetch and the geo lookup are injected so this stays free of app/extension state and
 remains unit-testable.
 */
public struct ExitNodeDirectory: Sendable {
    private let fetchRelays: @Sendable () async throws -> RelayListResponse
    private let lookupGeo: @Sendable (String) async -> ClientGeoInfo?
    private let selector: RelaySelector

    public init(
        fetchRelays: @escaping @Sendable () async throws -> RelayListResponse,
        lookupGeo: @escaping @Sendable (String) async -> ClientGeoInfo?,
        selector: RelaySelector = RelaySelector()
    ) {
        self.fetchRelays = fetchRelays
        self.lookupGeo = lookupGeo
        self.selector = selector
    }

    public func load() async throws -> [ExitNodeRegion] {
        let response = try await fetchRelays()
        let usable = selector.orderedCandidates(from: response.relays, now: response.serverTime)
        if usable.isEmpty { return [] }

        let hosts = Array(Set(usable.map(\.publicHost)))
        var geoByHost: [String: ClientGeoInfo] = [:]
        await withTaskGroup(of: (String, ClientGeoInfo?).self) { group in
            for host in hosts {
                group.addTask {
                    (host, await lookupGeo(host))
                }
            }

            for await (host, geo) in group {
                if let geo {
                    geoByHost[host] = geo
                }
            }
        }

        var grouped: [String: [ClientGeoInfo]] = [:]
        for relay in usable {
            guard let geo = geoByHost[relay.publicHost] else { continue }
            let code = CountryGeo.normalizedCountryCode(geo.countryCode)
            guard code.isEmpty == false else { continue }
            grouped[code, default: []].append(geo)
        }

        return grouped.map { code, geos in
            let centroid = CountryGeo.centroid(code)
            let first = geos[0]
            return ExitNodeRegion(
                countryCode: code,
                countryName: centroid?.name ?? (first.country.isEmpty ? code : first.country),
                latitude: centroid?.latitude ?? first.latitude,
                longitude: centroid?.longitude ?? first.longitude,
                nodeCount: geos.count
            )
        }
        .sorted { lhs, rhs in
            if lhs.nodeCount != rhs.nodeCount {
                return lhs.nodeCount > rhs.nodeCount
            }
            return lhs.countryName < rhs.countryName
        }
    }
}
