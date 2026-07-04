import Foundation

public struct ProxyEngineConfiguration: Codable, Equatable, Sendable {
    public let address: String
    public let port: Int
    public let userID: String
    public let flow: String
    public let security: String
    public let realityPublicKey: String
    public let shortID: String
    public let serverName: String

    public init(relay: RelayDescriptor) {
        self.address = relay.publicHost
        self.port = relay.publicPort
        self.userID = relay.clientID
        self.flow = relay.flow
        self.security = "reality"
        self.realityPublicKey = relay.realityPublicKey
        self.shortID = relay.shortID
        self.serverName = relay.serverName
    }

    enum CodingKeys: String, CodingKey {
        case address
        case port
        case userID = "user_id"
        case flow
        case security
        case realityPublicKey = "reality_public_key"
        case shortID = "short_id"
        case serverName = "server_name"
    }

    public func encodedJSON() throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return try encoder.encode(self)
    }
}
