import Foundation

public struct InternetProbeResult: Sendable, Equatable {
    public let endpoint: String
    public let durationMs: Int64
}

public enum InternetProbeError: Error {
    case unreachable(String?)
}

/// Verifies internet reachability through the active tunnel by hitting captive-portal
/// `generate_204` endpoints. Port of Android `InternetProbe`.
///
/// iOS divergence: the Android version binds requests to the VPN `Network`. iOS has no
/// equivalent in-extension API, so a plain `URLSession` request is used — inside the
/// PacketTunnel extension (after `setTunnelNetworkSettings`) that traffic egresses through
/// the tun, which is the behaviour this probe is meant to confirm.
public struct InternetProbe: Sendable {
    public static let defaultEndpoints = [
        "https://www.gstatic.com/generate_204",
        "https://cp.cloudflare.com/generate_204",
    ]

    private let endpoints: [String]
    private let session: URLSession
    private let deadlineMs: UInt64 = 12_000
    private let retryDelayNs: UInt64 = 500_000_000
    private let requestTimeout: TimeInterval = 3

    public init(endpoints: [String] = InternetProbe.defaultEndpoints, session: URLSession = .shared) {
        self.endpoints = endpoints
        self.session = session
    }

    public func verify() async throws -> InternetProbeResult {
        let startedNs = DispatchTime.now().uptimeNanoseconds
        let deadlineNs = startedNs + deadlineMs * 1_000_000
        var lastError: Error?

        while DispatchTime.now().uptimeNanoseconds < deadlineNs {
            for endpoint in endpoints {
                do {
                    try await probe(endpoint)
                    let elapsedMs = Int64((DispatchTime.now().uptimeNanoseconds - startedNs) / 1_000_000)
                    return InternetProbeResult(endpoint: endpoint, durationMs: elapsedMs)
                } catch is CancellationError {
                    throw CancellationError()
                } catch {
                    lastError = error
                }
            }
            try await Task.sleep(nanoseconds: retryDelayNs)
        }

        throw InternetProbeError.unreachable(lastError?.localizedDescription)
    }

    public static func acceptsHTTPStatus(_ status: Int) -> Bool {
        (200..<300).contains(status)
    }

    private func probe(_ endpoint: String) async throws {
        guard let url = URL(string: endpoint) else {
            throw URLError(.badURL)
        }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.timeoutInterval = requestTimeout
        request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        request.setValue("no-cache", forHTTPHeaderField: "Cache-Control")

        let (_, response) = try await session.data(for: request)
        let status = (response as? HTTPURLResponse)?.statusCode ?? -1
        guard InternetProbe.acceptsHTTPStatus(status) else {
            throw URLError(.badServerResponse)
        }
    }
}
