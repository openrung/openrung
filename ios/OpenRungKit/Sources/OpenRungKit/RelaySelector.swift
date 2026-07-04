import Foundation

public struct RelaySelector: Sendable {
    public init() {}

    public func orderedCandidates(from relays: [RelayDescriptor], now: Date = Date()) -> [RelayDescriptor] {
        relays.filter { $0.isUsable(now: now) }
    }

    public func selectFirstUsable(from relays: [RelayDescriptor], now: Date = Date()) -> RelayDescriptor? {
        orderedCandidates(from: relays, now: now).first
    }
}
