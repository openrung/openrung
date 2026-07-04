import Foundation

public struct SingBoxConfiguration: Equatable, Sendable {
    public let relay: RelayDescriptor
    public let tunnelIPv4Address: String
    public let tunnelIPv6Address: String
    public let dnsServers: [String]
    public let mtu: Int

    public init(
        relay: RelayDescriptor,
        tunnelIPv4Address: String = "172.19.0.1/30",
        tunnelIPv6Address: String = "fdfe:dcba:9876::1/126",
        dnsServers: [String] = ["1.1.1.1", "8.8.8.8"],
        mtu: Int = 1500
    ) {
        self.relay = relay
        self.tunnelIPv4Address = tunnelIPv4Address
        self.tunnelIPv6Address = tunnelIPv6Address
        self.dnsServers = dnsServers
        self.mtu = mtu
    }

    public func encodedJSON() throws -> Data {
        try JSONSerialization.data(
            withJSONObject: makeJSONObject(),
            options: [.prettyPrinted, .sortedKeys]
        )
    }

    public func encodedJSONString() throws -> String {
        String(decoding: try encodedJSON(), as: UTF8.self)
    }

    public func makeJSONObject() -> [String: Any] {
        var tunInbound: [String: Any] = [
            "type": "tun",
            "tag": "tun-in",
            "address": [
                tunnelIPv4Address,
                tunnelIPv6Address
            ],
            "mtu": mtu,
            "auto_route": true,
            "strict_route": true,
            "stack": "system",
            "dns_mode": "hijack",
            "endpoint_independent_nat": true
        ]
        if let excludeAddress = Self.relayRouteExcludeAddress(for: relay.publicHost) {
            tunInbound["route_exclude_address"] = [excludeAddress]
        }

        return [
            "log": [
                "level": "info",
                "timestamp": true
            ],
            "dns": [
                "servers": dnsServers.enumerated().map { index, server in
                    [
                        "tag": "dns-\(index)",
                        "type": "tcp",
                        "server": server,
                        "detour": "proxy"
                    ]
                },
                "final": "dns-0"
            ],
            "inbounds": [
                tunInbound
            ],
            "outbounds": [
                [
                    "type": "vless",
                    "tag": "proxy",
                    "server": relay.publicHost,
                    "server_port": relay.publicPort,
                    "uuid": relay.clientID,
                    "flow": relay.flow,
                    "network": "tcp",
                    "packet_encoding": "xudp",
                    "tls": [
                        "enabled": true,
                        "server_name": relay.serverName,
                        "utls": [
                            "enabled": true,
                            "fingerprint": "chrome"
                        ],
                        "reality": [
                            "enabled": true,
                            "public_key": relay.realityPublicKey,
                            "short_id": relay.shortID
                        ]
                    ] as [String: Any]
                ] as [String: Any],
                [
                    "type": "direct",
                    "tag": "direct"
                ],
                [
                    "type": "block",
                    "tag": "block"
                ]
            ],
            "route": [
                "auto_detect_interface": true,
                "default_domain_resolver": "dns-0",
                "rules": [
                    [
                        "protocol": "dns",
                        "action": "hijack-dns"
                    ]
                ],
                "final": "proxy"
            ]
        ]
    }

    private static func relayRouteExcludeAddress(for host: String) -> String? {
        let cleanHost = host.removingIPv6Brackets()
        if cleanHost.isIPv4Literal {
            return "\(cleanHost)/32"
        }
        if cleanHost.contains(":") {
            return "\(cleanHost)/128"
        }
        return nil
    }
}

private extension String {
    func removingIPv6Brackets() -> String {
        guard hasPrefix("["), hasSuffix("]") else {
            return self
        }
        return String(dropFirst().dropLast())
    }

    var isIPv4Literal: Bool {
        let octets = split(separator: ".", omittingEmptySubsequences: false)
        guard octets.count == 4 else {
            return false
        }
        return octets.allSatisfy { octet in
            guard let value = Int(octet), (0...255).contains(value) else {
                return false
            }
            return String(value) == String(octet)
        }
    }
}
