import Foundation

/// Stable per-install client identifier shared by the app and extension via the App Group.
/// Port of Android `ClientIdentity`.
enum ClientIdentity {
    private static let key = "client_id"

    static func getOrCreate() -> String {
        let defaults = UserDefaults(suiteName: AppConfig.appGroupIdentifier)
        if let existing = defaults?.string(forKey: key), existing.isEmpty == false {
            return existing
        }
        let identifier = UUID().uuidString
        defaults?.set(identifier, forKey: key)
        return identifier
    }
}
