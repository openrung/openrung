import Foundation

public enum TelemetryClientError: Error, Equatable {
    case httpStatus(Int)
}

/// Uploads telemetry event batches to the broker. Port of Android `TelemetryClient`.
public struct TelemetryClient: Sendable {
    private let url: URL
    private let session: URLSession

    public init(brokerURL: URL, session: URLSession = .shared) throws {
        self.url = try TelemetryClient.telemetryURL(brokerURL: brokerURL)
        self.session = session
    }

    public func send(_ events: [TelemetryEvent]) async throws {
        guard events.isEmpty == false else { return }

        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.timeoutInterval = 15
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue(events[0].clientId, forHTTPHeaderField: "X-OpenRung-Client-ID")
        request.setValue(events[0].sessionId, forHTTPHeaderField: "X-OpenRung-Session-ID")
        request.httpBody = try JSONEncoder().encode(TelemetryBatch(events: events))

        let (_, response) = try await session.data(for: request)
        if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) == false {
            throw TelemetryClientError.httpStatus(http.statusCode)
        }
    }

    public static func telemetryURL(brokerURL: URL) throws -> URL {
        try BrokerEndpoint.build(base: brokerURL, appending: "api/v1/telemetry/events")
    }
}
