import Foundation

enum TunnelDiagnostics {
    private static let lastEventKey = "tunnel_diagnostics_last_event"
    private static let lastErrorKey = "tunnel_diagnostics_last_error"
    private static let updatedAtKey = "tunnel_diagnostics_updated_at"

    static func clear() {
        guard let defaults = defaults else {
            return
        }
        defaults.removeObject(forKey: lastEventKey)
        defaults.removeObject(forKey: lastErrorKey)
        defaults.removeObject(forKey: updatedAtKey)
    }

    static func recordEvent(_ message: String) {
        guard let defaults = defaults else {
            return
        }
        defaults.set(message, forKey: lastEventKey)
        defaults.set(Date().timeIntervalSince1970, forKey: updatedAtKey)
    }

    static func recordError(_ message: String) {
        guard let defaults = defaults else {
            return
        }
        defaults.set(message, forKey: lastErrorKey)
        defaults.set(message, forKey: lastEventKey)
        defaults.set(Date().timeIntervalSince1970, forKey: updatedAtKey)
    }

    static func latestSummary() -> String? {
        guard let defaults = defaults else {
            return nil
        }

        let message = defaults.string(forKey: lastErrorKey)
            ?? defaults.string(forKey: lastEventKey)
        guard let message, message.isEmpty == false else {
            return nil
        }

        return message
    }

    private static var defaults: UserDefaults? {
        UserDefaults(suiteName: AppConfig.appGroupIdentifier)
    }
}
