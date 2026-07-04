import Foundation
import OpenRungKit

/// App-group-backed connection state shared between the PacketTunnel extension (the writer) and
/// the SwiftUI app (the reader). Each mutation persists a `ConnectionStateSnapshot` to the shared
/// `UserDefaults` and posts a Darwin notification so the app can re-read. Port of the cross-process
/// half of Android `OpenRungStatusStore`.
enum SharedConnectionState {
    private static let key = "connection_state"

    private static var defaults: UserDefaults? {
        UserDefaults(suiteName: AppConfig.appGroupIdentifier)
    }

    static func snapshot() -> ConnectionStateSnapshot {
        guard
            let data = defaults?.data(forKey: key),
            let snapshot = try? JSONDecoder().decode(ConnectionStateSnapshot.self, from: data)
        else {
            return ConnectionStateSnapshot(brokerURL: AppConfig.defaultBrokerURL.absoluteString)
        }
        return snapshot
    }

    /// What the app shows on a cold launch: a stale CONNECTED never survives, and the relay label
    /// (which could leak a prior relay) is dropped until re-resolved.
    static func sanitizedForColdStart() -> ConnectionStateSnapshot {
        var snapshot = snapshot()
        if snapshot.status == .connected {
            snapshot.status = .disconnected
        }
        snapshot.relayLabel = nil
        return snapshot
    }

    // MARK: - Mutators (called by the extension)

    static func setBrokerURL(_ url: String) {
        mutate { $0.brokerURL = url }
    }

    static func setRelayLabel(_ label: String?) {
        mutate { $0.relayLabel = label }
    }

    static func recordRecent(_ node: RecentNode) {
        mutate { snapshot in
            snapshot.recentRegions = ([node] + snapshot.recentRegions.filter { $0.countryCode != node.countryCode })
                .prefix(AppConfig.maxRecents)
                .map { $0 }
        }
    }

    static func clearError() {
        mutate { $0.lastError = nil }
    }

    static func setStatus(_ status: ConnectionStatus, clearRelayLabel: Bool = false, clearError: Bool = false) {
        mutate { snapshot in
            snapshot.status = status
            if clearRelayLabel { snapshot.relayLabel = nil }
            if clearError { snapshot.lastError = nil }
            snapshot.logLines = ActivityLog.appended(snapshot.logLines, ActivityLog.line(status.displayLabel))
        }
    }

    static func appendLog(_ message: String) {
        mutate { snapshot in
            snapshot.logLines = ActivityLog.appended(snapshot.logLines, ActivityLog.line(message))
        }
    }

    static func fail(_ message: String) {
        mutate { snapshot in
            snapshot.status = .failed
            snapshot.lastError = message
            snapshot.relayLabel = nil
            snapshot.logLines = ActivityLog.appended(snapshot.logLines, ActivityLog.line("error: \(message)"))
        }
    }

    // MARK: - Persistence + notification

    private static func mutate(_ transform: (inout ConnectionStateSnapshot) -> Void) {
        var snapshot = snapshot()
        transform(&snapshot)
        guard let data = try? JSONEncoder().encode(snapshot) else { return }
        defaults?.set(data, forKey: key)
        postDarwinNotification()
    }

    static func postDarwinNotification() {
        CFNotificationCenterPostNotification(
            CFNotificationCenterGetDarwinNotifyCenter(),
            CFNotificationName(AppConfig.darwinNotificationName as CFString),
            nil,
            nil,
            true
        )
    }
}
