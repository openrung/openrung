import Foundation

/// User-facing strings, mirroring the Android `res/values/strings.xml` (English). Kept in one place
/// so localization can be layered on later.
enum Strings {
    static let mainTitle = "openrung://mobile-client"
    static let readyLog = "ready. tap connect to route through a volunteer relay."
    static let actionConnect = "CONNECT"
    static let actionDisconnect = "DISCONNECT"
    static let trafficRouteConnected = "traffic route: device -> OpenRung VPN -> volunteer relay"
    static let trafficRouteDisconnected = "vpn is fail-closed: no relay, no connection."
    static let mapLoading = "locating available exit nodes..."
    static let mapFailed = "couldn't load exit nodes - tap to retry"
    static let mapNoNodes = "no exit nodes available right now"
    static let recentsLabel = "Recents"
    static let recentsEmpty = "No recent locations yet."

    static let settingsTitle = "Settings"
    static let versionTitle = "Version"
    static let debugSettingTitle = "Debug"
    static let debugSettingSubtitle = "Connection console and diagnostics."
    static let debugTitle = "Debug console"
    static let speedTestTitle = "Volunteer speed test"
    static let speedTestReady = "Download 10 MB through the active volunteer relay and report the result."
    static let speedTestRequiresConnection = "Connect to a volunteer relay before running the speed test."
    static let speedTestRunning = "Testing download speed through the volunteer relay…"
    static let speedTestAction = "RUN"
    static let relayLocationUnknown = "Unknown location"

    static let sourceURL = "https://github.com/openrung/openrung"
    static let licensesSettingTitle = "Open-source licenses"
    static let licensesSettingSubtitle = "Licenses and attribution for bundled software."
    static let licensesTitle = "Open-source licenses"
    static let licensesIntro = "OpenRung is free software licensed under GPL-3.0-or-later because it links sing-box. The complete corresponding source for this build is available at the link below."
    static let licensesSourceTitle = "Source code"
    static let licensesFullTextTitle = "Full license texts"
    static let licensesFullTextSubtitle = "GNU GPL-3.0 and third-party notices."
    static let licensesComponentsHeader = "Components"

    static func status(_ value: String) -> String { "status = \(value)" }
    static func relay(_ value: String) -> String { "relay = \(value)" }
    static func logLine(_ value: String) -> String { "> \(value)" }
    static func errorLine(_ value: String) -> String { "! \(value)" }
    static func mapNodesAvailable(_ count: Int) -> String { "\(count) locations available" }
    static func speedTestResult(_ mbps: Double) -> String { "Download speed: \(String(format: "%.1f", mbps)) Mbps" }
    static func speedTestError(_ message: String) -> String { "Speed test failed: \(message)" }
}
