import Foundation

/// Pure helpers for the activity-log ring buffer. Mirrors Android `OpenRungStatusStore.appendLog`
/// (timestamped `[HH:mm:ss] message` lines, capped at 80).
public enum ActivityLog {
    public static let maxLines = 80

    public static func line(_ message: String, at date: Date = Date()) -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "HH:mm:ss"
        return "[\(formatter.string(from: date))] \(message)"
    }

    public static func appended(_ lines: [String], _ line: String, max: Int = maxLines) -> [String] {
        Array((lines + [line]).suffix(max))
    }
}
