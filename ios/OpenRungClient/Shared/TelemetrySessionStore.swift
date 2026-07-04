import Foundation
import OpenRungKit

/// Persists the current telemetry `Session` to the App Group so the app process can attach
/// speed-test events to the live session created by the extension. Port of the session half of
/// Android `TelemetryManager`.
enum TelemetrySessionStore {
    private static let key = "telemetry_session"

    private static var defaults: UserDefaults? {
        UserDefaults(suiteName: AppConfig.appGroupIdentifier)
    }

    static func current() -> TelemetrySession? {
        guard let data = defaults?.data(forKey: key) else { return nil }
        return try? JSONDecoder().decode(TelemetrySession.self, from: data)
    }

    static func save(_ session: TelemetrySession?) {
        guard let session else {
            defaults?.removeObject(forKey: key)
            return
        }
        if let data = try? JSONEncoder().encode(session) {
            defaults?.set(data, forKey: key)
        }
    }
}
