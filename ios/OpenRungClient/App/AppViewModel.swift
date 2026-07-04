import Foundation
import NetworkExtension
import OpenRungKit

/// App-side state holder. Manages the `NETunnelProviderManager`, reflects the rich connection state
/// the extension publishes via `SharedConnectionState` (re-read on a Darwin notification), and runs
/// the user-triggered speed test through the active tunnel.
@MainActor
final class AppViewModel: ObservableObject {
    @Published private(set) var status: ConnectionStatus = .disconnected
    @Published private(set) var relayLabel: String?
    @Published private(set) var lastError: String?
    @Published private(set) var logLines: [String] = []
    @Published private(set) var availableRegions: [ExitNodeRegion] = []
    @Published private(set) var recentRegions: [RecentNode] = []
    @Published private(set) var directoryStatus: DirectoryStatus = .idle
    @Published private(set) var isBusy = false

    let brokerURL = AppConfig.defaultBrokerURL

    private var manager: NETunnelProviderManager?
    private var vpnStatus: NEVPNStatus = .invalid
    private var directoryTask: Task<Void, Never>?

    var isConnected: Bool { status.isConnected }
    var isWorking: Bool { status.isWorking || isBusy }
    var isActive: Bool { isConnected || isWorking }

    init() {
        apply(SharedConnectionState.sanitizedForColdStart())
        observeVPNStatus()
        observeSharedState()
    }

    deinit {
        directoryTask?.cancel()
        CFNotificationCenterRemoveEveryObserver(
            CFNotificationCenterGetDarwinNotifyCenter(),
            Unmanaged.passUnretained(self).toOpaque()
        )
    }

    // MARK: - Lifecycle

    func load() async {
        guard Self.canUseNetworkExtension else { return }
        do {
            manager = try await loadOrCreateManager()
            refreshVPNStatus()
            refreshDirectory()
        } catch {
            lastError = AppError.message(for: error)
        }
    }

    func toggle() async {
        if isActive {
            await disconnect()
        } else {
            await connect()
        }
    }

    func connect(countryCode: String? = nil) async {
        let targetCountry = Self.normalizedCountryCode(countryCode)
        let shouldSwitchRelay = isConnected || status.isWorking
        isBusy = true
        defer { isBusy = false }
        do {
            guard Self.canUseNetworkExtension else {
                throw AppError.networkExtensionUnavailableInSimulator
            }
            let manager = try await loadOrCreateManager()
            if shouldSwitchRelay {
                manager.connection.stopVPNTunnel()
                refreshVPNStatus()
                try? await Task.sleep(nanoseconds: 350_000_000)
            }
            try await configure(manager: manager, targetCountry: targetCountry)
            try manager.connection.startVPNTunnel()
            self.manager = manager
            refreshVPNStatus()
        } catch {
            lastError = AppError.message(for: error)
            status = .failed
        }
    }

    func disconnect() async {
        manager?.connection.stopVPNTunnel()
        refreshVPNStatus()
    }

    func refreshDirectory(force: Bool = false) {
        let alreadyLoaded = directoryStatus == .loaded && availableRegions.isEmpty == false
        if !force && (directoryStatus == .loading || alreadyLoaded) { return }

        directoryTask?.cancel()
        directoryStatus = .loading
        let primaryBrokerURL = brokerURL
        directoryTask = Task {
            let directory = ExitNodeDirectory(
                fetchRelays: {
                    try await BrokerClient.firstReachable(
                        candidates: AppConfig.brokerCandidates(primary: primaryBrokerURL),
                        limit: AppConfig.directoryRelayLimit
                    ).response
                },
                lookupGeo: { host in
                    try? await GeoIpClient().lookup(ip: host)
                }
            )
            do {
                let regions = try await directory.load()
                guard Task.isCancelled == false else { return }
                availableRegions = regions
                directoryStatus = .loaded
            } catch is CancellationError {
                return
            } catch {
                guard Task.isCancelled == false else { return }
                directoryStatus = .failed
            }
        }
    }

    // MARK: - Speed test (runs through the tunnel from the app process)

    func runSpeedTest() async -> Result<SpeedTestResult, Error> {
        guard let session = TelemetryManager.activeSession(), let url = URL(string: session.brokerURL) else {
            return .failure(AppError.noActiveSession)
        }
        do {
            let result = try await SpeedTestClient(brokerURL: url).run()
            TelemetryManager.recordSpeedTest(result)
            try? await TelemetryManager.flush(brokerURL: session.brokerURL)
            return .success(result)
        } catch {
            TelemetryManager.record(
                "speed_test_failed",
                attributes: ["provider": "openrung_broker", "error_type": String(describing: type(of: error))]
            )
            try? await TelemetryManager.flush(brokerURL: session.brokerURL)
            return .failure(error)
        }
    }

    // MARK: - Manager plumbing

    private func loadOrCreateManager() async throws -> NETunnelProviderManager {
        let managers = try await NETunnelProviderManager.loadAllFromPreferences()
        if let existing = managers.first(where: { $0.localizedDescription == AppConfig.vpnProfileName }) {
            return existing
        }
        let manager = NETunnelProviderManager()
        manager.localizedDescription = AppConfig.vpnProfileName
        return manager
    }

    private func configure(manager: NETunnelProviderManager, targetCountry: String?) async throws {
        let tunnelProtocol = NETunnelProviderProtocol()
        tunnelProtocol.providerBundleIdentifier = AppConfig.packetTunnelBundleIdentifier
        tunnelProtocol.serverAddress = brokerURL.host ?? brokerURL.absoluteString
        var providerConfiguration = [AppConfig.providerBrokerURLKey: brokerURL.absoluteString]
        if let targetCountry {
            providerConfiguration[AppConfig.providerTargetCountryKey] = targetCountry
        }
        tunnelProtocol.providerConfiguration = providerConfiguration
        manager.protocolConfiguration = tunnelProtocol
        manager.isEnabled = true
        try await manager.saveToPreferences()
        try await manager.loadFromPreferences()
    }

    // MARK: - State observation

    private func observeVPNStatus() {
        NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            Task { @MainActor in self?.refreshVPNStatus() }
        }
    }

    private func observeSharedState() {
        let observer = Unmanaged.passUnretained(self).toOpaque()
        CFNotificationCenterAddObserver(
            CFNotificationCenterGetDarwinNotifyCenter(),
            observer,
            { _, observer, _, _, _ in
                guard let observer else { return }
                let viewModel = Unmanaged<AppViewModel>.fromOpaque(observer).takeUnretainedValue()
                Task { @MainActor in viewModel.reloadSharedState() }
            },
            AppConfig.darwinNotificationName as CFString,
            nil,
            .deliverImmediately
        )
    }

    private func reloadSharedState() {
        apply(SharedConnectionState.snapshot())
    }

    private func refreshVPNStatus() {
        vpnStatus = manager?.connection.status ?? .invalid
        reloadSharedState()
        // If the OS reports the tunnel is fully down but the extension's last write was optimistic
        // (e.g. it was killed without recording a terminal state), reflect disconnected.
        if vpnStatus == .disconnected || vpnStatus == .invalid,
           status == .connected || status == .connecting || status == .preparing {
            status = .disconnected
            relayLabel = nil
        }
    }

    private func apply(_ snapshot: ConnectionStateSnapshot) {
        status = snapshot.status
        relayLabel = snapshot.relayLabel
        lastError = snapshot.lastError
        logLines = snapshot.logLines
        recentRegions = snapshot.recentRegions
    }

    private static var canUseNetworkExtension: Bool {
        #if targetEnvironment(simulator)
        false
        #else
        true
        #endif
    }

    private static func normalizedCountryCode(_ countryCode: String?) -> String? {
        guard let normalized = countryCode?.trimmingCharacters(in: .whitespacesAndNewlines).uppercased(),
              normalized.isEmpty == false
        else {
            return nil
        }
        return normalized
    }
}

enum AppError: LocalizedError {
    case networkExtensionUnavailableInSimulator
    case noActiveSession

    var errorDescription: String? {
        switch self {
        case .networkExtensionUnavailableInSimulator:
            return "The iOS simulator cannot install or start a Packet Tunnel VPN profile. Run on a signed physical iPhone with the Network Extension packet-tunnel entitlement to test Connect."
        case .noActiveSession:
            return "No active VPN session. Connect to a volunteer relay first."
        }
    }

    static func message(for error: Error) -> String {
        let nsError = error as NSError
        if nsError.domain == "NEConfigurationErrorDomain", nsError.code == 11 {
            return "Network Extension preferences are unavailable. On the simulator this is expected; on a real iPhone, confirm the app and Packet Tunnel extension are signed with the packet-tunnel entitlement and matching App Group."
        }
        return (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
    }
}
