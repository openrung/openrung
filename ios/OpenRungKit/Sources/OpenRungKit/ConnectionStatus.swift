import Foundation

/// Connection lifecycle states. Port of Android `ConnectionStatus`.
public enum ConnectionStatus: String, Codable, Sendable, CaseIterable {
    case disconnected
    case preparing
    case connecting
    case connected
    case disconnecting
    case failed

    public var isWorking: Bool {
        self == .preparing || self == .connecting || self == .disconnecting
    }

    public var isConnected: Bool {
        self == .connected
    }

    /// Default English label (matches Android `status_*` strings).
    public var displayLabel: String {
        switch self {
        case .disconnected: return "Disconnected"
        case .preparing: return "Preparing VPN"
        case .connecting: return "Connecting"
        case .connected: return "Connected"
        case .disconnecting: return "Disconnecting"
        case .failed: return "Failed"
        }
    }
}

/// The connection UI state shared between the PacketTunnel extension (writer) and the app (reader).
/// Port of Android `OpenRungUiState`.
public struct ConnectionStateSnapshot: Codable, Sendable, Equatable {
    public var status: ConnectionStatus
    public var brokerURL: String
    public var relayLabel: String?
    public var lastError: String?
    public var logLines: [String]
    public var recentRegions: [RecentNode]

    public init(
        status: ConnectionStatus = .disconnected,
        brokerURL: String = "",
        relayLabel: String? = nil,
        lastError: String? = nil,
        logLines: [String] = [],
        recentRegions: [RecentNode] = []
    ) {
        self.status = status
        self.brokerURL = brokerURL
        self.relayLabel = relayLabel
        self.lastError = lastError
        self.logLines = logLines
        self.recentRegions = recentRegions
    }

    public var isWorking: Bool { status.isWorking }
    public var isConnected: Bool { status.isConnected }

    enum CodingKeys: String, CodingKey {
        case status
        case brokerURL
        case relayLabel
        case lastError
        case logLines
        case recentRegions
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        status = try container.decodeIfPresent(ConnectionStatus.self, forKey: .status) ?? .disconnected
        brokerURL = try container.decodeIfPresent(String.self, forKey: .brokerURL) ?? ""
        relayLabel = try container.decodeIfPresent(String.self, forKey: .relayLabel)
        lastError = try container.decodeIfPresent(String.self, forKey: .lastError)
        logLines = try container.decodeIfPresent([String].self, forKey: .logLines) ?? []
        recentRegions = try container.decodeIfPresent([RecentNode].self, forKey: .recentRegions) ?? []
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(status, forKey: .status)
        try container.encode(brokerURL, forKey: .brokerURL)
        try container.encodeIfPresent(relayLabel, forKey: .relayLabel)
        try container.encodeIfPresent(lastError, forKey: .lastError)
        try container.encode(logLines, forKey: .logLines)
        try container.encode(recentRegions, forKey: .recentRegions)
    }
}
