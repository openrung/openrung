import Foundation

public enum BrokerClientError: Error, Equatable {
    case invalidResponse
    case httpStatus(Int)
}

/// A successful relay fetch together with the broker endpoint that served it.
public struct BrokerFetch: Sendable {
    public let brokerURL: URL
    public let response: RelayListResponse

    public init(brokerURL: URL, response: RelayListResponse) {
        self.brokerURL = brokerURL
        self.response = response
    }
}

public struct BrokerClient: Sendable {
    private let baseURL: URL
    private let session: URLSession
    private let decoder: JSONDecoder

    public init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session

        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        self.decoder = decoder
    }

    public func listRelays(limit: Int = 5, clientID: String? = nil, sessionID: String? = nil) async throws -> RelayListResponse {
        let url = try BrokerClient.relaysURL(brokerURL: baseURL, limit: limit)

        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        if let clientID {
            request.setValue(clientID, forHTTPHeaderField: "X-OpenRung-Client-ID")
        }
        if let sessionID {
            request.setValue(sessionID, forHTTPHeaderField: "X-OpenRung-Session-ID")
        }
        request.setValue(DeviceAttributes.appVersion, forHTTPHeaderField: "X-OpenRung-App-Version")
        request.setValue(DeviceAttributes.osVersion, forHTTPHeaderField: "X-OpenRung-iOS-Version")

        let (data, response) = try await session.data(for: request)
        guard let httpResponse = response as? HTTPURLResponse else {
            throw BrokerClientError.invalidResponse
        }
        guard (200..<300).contains(httpResponse.statusCode) else {
            throw BrokerClientError.httpStatus(httpResponse.statusCode)
        }

        return try decoder.decode(RelayListResponse.self, from: data)
    }

    /// Builds the ordered broker candidate list, de-duplicated while preserving order. `primary` is
    /// tried FIRST only when it is a genuine override — i.e. not already one of the `fallbacks`. A
    /// persisted value that merely echoes a built-in default must NOT reorder the defaults' preferred
    /// (HTTPS-first) ordering. Pure and side-effect free so it is unit-testable.
    public static func candidates(primary: URL?, fallbacks: [URL]) -> [URL] {
        var ordered: [URL] = []
        if let primary, fallbacks.contains(primary) == false {
            ordered.append(primary)
        }
        for fallback in fallbacks where ordered.contains(fallback) == false {
            ordered.append(fallback)
        }
        return ordered
    }

    /// Fetches relays from each candidate broker in order, returning the first success along with the
    /// endpoint that served it. A blocked or down primary endpoint therefore no longer takes discovery
    /// offline as long as one candidate is reachable. Honors task cancellation; if every candidate
    /// fails, the last error is rethrown.
    public static func firstReachable(
        candidates: [URL],
        limit: Int = 5,
        clientID: String? = nil,
        sessionID: String? = nil,
        session: URLSession = .shared
    ) async throws -> BrokerFetch {
        var lastError: Error?
        for url in candidates {
            try Task.checkCancellation()
            do {
                let response = try await BrokerClient(baseURL: url, session: session)
                    .listRelays(limit: limit, clientID: clientID, sessionID: sessionID)
                return BrokerFetch(brokerURL: url, response: response)
            } catch {
                lastError = error
            }
        }
        throw lastError ?? BrokerClientError.invalidResponse
    }

    public static func relaysURL(brokerURL: URL, limit: Int) throws -> URL {
        let base = try BrokerEndpoint.build(base: brokerURL, appending: "api/v1/relays")
        guard var components = URLComponents(url: base, resolvingAgainstBaseURL: false) else {
            throw URLError(.badURL)
        }
        components.queryItems = [URLQueryItem(name: "limit", value: String(max(limit, 1)))]
        guard let url = components.url else {
            throw URLError(.badURL)
        }
        return url
    }
}
