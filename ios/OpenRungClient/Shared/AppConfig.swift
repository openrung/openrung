import Foundation
import OpenRungKit

enum AppConfig {
    static let vpnProfileName = "OpenRung Volunteer VPN"
    static let appGroupIdentifier = "group.com.openrung.client"
    static let packetTunnelBundleIdentifier = "com.openrung.client.PacketTunnel"
    static let providerBrokerURLKey = "broker_url"
    static let providerTargetCountryKey = "target_country"

    /// Discovery broker (relay-list bootstrap) default. Prefer the HTTPS, Cloudflare-fronted endpoint:
    /// discovery is the censorship-critical path — it runs BEFORE the VPN tunnel is up — and TLS + CDN
    /// edge IPs make it costly to block. Falls back to the raw origin IP; see `defaultBrokerURLs`.
    static let defaultBrokerURL = URL(string: "https://broker.openrung.org/")!

    /// Telemetry / heartbeat / speed-test target: the raw origin IP, NOT the Cloudflare-fronted
    /// hostname. Heartbeats fire ~once/minute per connected user; routing them through the Cloudflare
    /// Worker would burn the Workers free-tier quota (100k requests/day). They ride the established VPN
    /// tunnel so they don't need the CDN front (and it's the same broker either way). Discovery stays
    /// fronted (low volume, pre-tunnel, needs the resilience); telemetry goes direct (high volume).
    static let telemetryBrokerURL = URL(string: "http://54.238.185.205:8080/")!

    /// Ordered discovery candidates, tried in order until one returns relays (see
    /// `BrokerClient.firstReachable`): the Cloudflare-fronted endpoint first, then the raw IP as a
    /// fallback so a blocked edge never takes discovery offline. The raw cleartext IP is why
    /// `NSAllowsArbitraryLoads` is still set.
    static let defaultBrokerURLs: [URL] = [defaultBrokerURL, telemetryBrokerURL]

    /// Ordered broker candidates for a connection attempt: the caller-selected `primary` (the provider
    /// configuration's broker, today the default) first, then the built-in `defaultBrokerURLs`,
    /// de-duplicated while preserving order.
    static func brokerCandidates(primary: URL?) -> [URL] {
        BrokerClient.candidates(primary: primary, fallbacks: defaultBrokerURLs)
    }
    static let loggingSubsystem = "com.openrung.client.PacketTunnel"
    static let engineDirectoryName = "OpenRungPacketTunnel"
    static let relayLimit = 5
    static let directoryRelayLimit = 20
    static let maxRecents = 8

    // App ↔ extension shared-state plumbing.
    static let darwinNotificationName = "com.openrung.client.state-changed"
    static let telemetryOutboxFilename = "outbox.json"

    // Heartbeat cadence (random in this range), matching Android.
    static let heartbeatMinDelayMs: UInt64 = 50_000
    static let heartbeatMaxDelayMs: UInt64 = 70_000
}
