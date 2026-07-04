import Foundation
#if canImport(UIKit)
import UIKit
#endif

/// iOS analog of the Android `TelemetryManager.deviceAttributes()` map.
public enum DeviceAttributes {
    /// The host app/extension short version (`CFBundleShortVersionString`).
    public static var appVersion: String {
        (Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String) ?? "unknown"
    }

    /// iOS version string (replaces Android's `android_api`).
    public static var osVersion: String {
        #if canImport(UIKit)
        return UIDevice.current.systemVersion
        #else
        let version = ProcessInfo.processInfo.operatingSystemVersion
        return "\(version.majorVersion).\(version.minorVersion).\(version.patchVersion)"
        #endif
    }

    /// Hardware identifier such as `iPhone15,2`.
    public static var deviceModel: String {
        var systemInfo = utsname()
        uname(&systemInfo)
        let model = withUnsafeBytes(of: &systemInfo.machine) { raw -> String in
            let bytes = Data(raw)
            let trimmed = bytes.prefix { $0 != 0 }
            return String(decoding: trimmed, as: UTF8.self)
        }
        return model.isEmpty ? "unknown" : model
    }

    /// Best-effort attribute map attached to every telemetry event.
    public static func current() -> [String: String] {
        NetworkPathMonitor.shared.startIfNeeded()
        let path = NetworkPathMonitor.shared.currentSnapshot
        return [
            "app_version": appVersion,
            "ios_version": osVersion,
            "device_manufacturer": "Apple",
            "device_model": deviceModel,
            "locale": Locale.current.identifier,
            "timezone": TimeZone.current.identifier,
            "network_transport": path.transport,
            "network_metered": String(path.isExpensive),
        ]
    }
}
