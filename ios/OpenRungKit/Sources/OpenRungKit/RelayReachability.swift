import Foundation
import Network

public enum RelayReachabilityError: Error, Equatable {
    case invalidPort
    case timeout
}

/// Measures TCP connect latency to a relay endpoint. Port of Android `RelayReachability.checkTcp`.
public enum RelayReachability {
    public static func checkTcp(_ relay: RelayDescriptor, timeoutMillis: Int = 5_000) async throws -> Int64 {
        try await checkTcp(host: relay.publicHost, port: relay.publicPort, timeoutMillis: timeoutMillis)
    }

    public static func checkTcp(host rawHost: String, port: Int, timeoutMillis: Int = 5_000) async throws -> Int64 {
        let host = rawHost
            .trimmingCharacters(in: .whitespacesAndNewlines)
            .replacingOccurrences(of: "[", with: "")
            .replacingOccurrences(of: "]", with: "")
        guard port > 0, port <= 65_535, let nwPort = NWEndpoint.Port(rawValue: UInt16(port)) else {
            throw RelayReachabilityError.invalidPort
        }

        let connection = NWConnection(host: NWEndpoint.Host(host), port: nwPort, using: .tcp)
        let queue = DispatchQueue(label: "com.openrung.client.reachability")
        let startedNs = DispatchTime.now().uptimeNanoseconds

        final class ResumeGuard: @unchecked Sendable {
            private let lock = NSLock()
            private var resumed = false
            func claim() -> Bool {
                lock.lock()
                defer { lock.unlock() }
                if resumed { return false }
                resumed = true
                return true
            }
        }
        let guardBox = ResumeGuard()

        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Int64, Error>) in
                func finish(_ result: Result<Int64, Error>) {
                    guard guardBox.claim() else { return }
                    connection.stateUpdateHandler = nil
                    connection.cancel()
                    continuation.resume(with: result)
                }

                connection.stateUpdateHandler = { state in
                    switch state {
                    case .ready:
                        let elapsedMs = Int64((DispatchTime.now().uptimeNanoseconds - startedNs) / 1_000_000)
                        finish(.success(elapsedMs))
                    case .failed(let error):
                        finish(.failure(error))
                    case .waiting(let error):
                        finish(.failure(error))
                    default:
                        break
                    }
                }

                queue.asyncAfter(deadline: .now() + .milliseconds(timeoutMillis)) {
                    finish(.failure(RelayReachabilityError.timeout))
                }
                connection.start(queue: queue)
            }
        } onCancel: {
            connection.cancel()
        }
    }
}
