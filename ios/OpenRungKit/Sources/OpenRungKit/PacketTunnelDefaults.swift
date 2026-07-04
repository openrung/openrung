import Foundation

public struct PacketTunnelDefaults: Equatable, Sendable {
    public let ipv4Address: String
    public let ipv4SubnetMask: String
    public let dnsServers: [String]
    public let mtu: Int

    public init(
        ipv4Address: String = "10.77.0.2",
        ipv4SubnetMask: String = "255.255.255.0",
        dnsServers: [String] = ["1.1.1.1", "8.8.8.8"],
        mtu: Int = 1500
    ) {
        self.ipv4Address = ipv4Address
        self.ipv4SubnetMask = ipv4SubnetMask
        self.dnsServers = dnsServers
        self.mtu = mtu
    }
}
