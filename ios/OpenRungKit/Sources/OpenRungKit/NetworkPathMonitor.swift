import Foundation
import Network

public struct NetworkPathSnapshot: Sendable, Equatable {
    public let transport: String
    public let isExpensive: Bool
    public let isConstrained: Bool

    public static let unknown = NetworkPathSnapshot(transport: "unknown", isExpensive: false, isConstrained: false)
}

/// Process-wide network path observer. Mirrors the role of Android's `ConnectivityManager`
/// lookups used by `TelemetryManager.deviceAttributes`. Starts a single `NWPathMonitor`
/// on first use and caches the latest snapshot for synchronous reads.
public final class NetworkPathMonitor: @unchecked Sendable {
    public static let shared = NetworkPathMonitor()

    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "com.openrung.client.networkpath")
    private let lock = NSLock()
    private var snapshot: NetworkPathSnapshot = .unknown
    private var started = false

    private init() {}

    public func startIfNeeded() {
        lock.lock()
        let alreadyStarted = started
        started = true
        lock.unlock()

        guard alreadyStarted == false else { return }
        monitor.pathUpdateHandler = { [weak self] path in
            self?.update(path)
        }
        monitor.start(queue: queue)
    }

    public var currentSnapshot: NetworkPathSnapshot {
        lock.lock()
        defer { lock.unlock() }
        return snapshot
    }

    private func update(_ path: NWPath) {
        let transport: String
        if path.status != .satisfied {
            transport = "unknown"
        } else if path.usesInterfaceType(.wifi) {
            transport = "wifi"
        } else if path.usesInterfaceType(.cellular) {
            transport = "cellular"
        } else if path.usesInterfaceType(.wiredEthernet) {
            transport = "ethernet"
        } else {
            transport = "other"
        }

        lock.lock()
        snapshot = NetworkPathSnapshot(
            transport: transport,
            isExpensive: path.isExpensive,
            isConstrained: path.isConstrained
        )
        lock.unlock()
    }
}
