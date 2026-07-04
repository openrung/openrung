import Foundation

/// Builds broker API endpoint URLs while preserving any base path and port,
/// mirroring the Android clients' URL composition (e.g. `relayListUrl`, `speedTestUrl`).
enum BrokerEndpoint {
    static func build(base: URL, appending pathComponent: String) throws -> URL {
        guard var components = URLComponents(url: base, resolvingAgainstBaseURL: false) else {
            throw URLError(.badURL)
        }
        let basePath = components.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        components.path = "/" + [basePath, pathComponent]
            .filter { $0.isEmpty == false }
            .joined(separator: "/")
        components.query = nil
        components.fragment = nil

        guard let url = components.url else {
            throw URLError(.badURL)
        }
        return url
    }
}
