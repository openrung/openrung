#if canImport(Libbox)
import Foundation
import Libbox
import Network
import NetworkExtension
import OSLog

final class LibboxPacketTunnelPlatformInterface: NSObject, LibboxPlatformInterfaceProtocol, LibboxCommandServerHandlerProtocol {
    private let tunnelProvider: NEPacketTunnelProvider
    private let logger = Logger(subsystem: AppConfig.loggingSubsystem, category: "LibboxPlatformInterface")
    private var networkSettings: NEPacketTunnelNetworkSettings?
    private var pathMonitor: NWPathMonitor?

    init(tunnelProvider: NEPacketTunnelProvider) {
        self.tunnelProvider = tunnelProvider
    }

    func openTun(_ options: LibboxTunOptionsProtocol?, ret0_: UnsafeMutablePointer<Int32>?) throws {
        try runBlocking {
            try await self.openTunAsync(options, ret0_)
        }
    }

    private func openTunAsync(_ options: LibboxTunOptionsProtocol?, _ ret0_: UnsafeMutablePointer<Int32>?) async throws {
        TunnelDiagnostics.recordEvent("libbox requested TUN open")
        logger.info("libbox requested TUN open")
        guard let options else {
            throw PacketTunnelProxyEngineError.engineStartFailed("Missing libbox TUN options.")
        }
        guard let ret0_ else {
            throw PacketTunnelProxyEngineError.engineStartFailed("Missing libbox TUN return pointer.")
        }

        let settings = try makeNetworkSettings(options)
        networkSettings = settings
        TunnelDiagnostics.recordEvent("Applying packet tunnel network settings")
        logger.info("Applying packet tunnel network settings")
        try await tunnelProvider.setTunnelNetworkSettings(settings)

        if let tunFd = tunnelProvider.packetFlow.value(forKeyPath: "socket.fileDescriptor") as? Int32 {
            TunnelDiagnostics.recordEvent("Resolved packet tunnel file descriptor via packetFlow")
            logger.info("Resolved packet tunnel file descriptor via packetFlow")
            ret0_.pointee = tunFd
            return
        }

        let fallbackFd = LibboxGetTunnelFileDescriptor()
        guard fallbackFd != -1 else {
            throw PacketTunnelProxyEngineError.engineStartFailed("Unable to resolve packet tunnel file descriptor.")
        }

        TunnelDiagnostics.recordEvent("Resolved packet tunnel file descriptor via libbox fallback")
        logger.info("Resolved packet tunnel file descriptor via libbox fallback")
        ret0_.pointee = fallbackFd
    }

    private func makeNetworkSettings(_ options: LibboxTunOptionsProtocol) throws -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        settings.mtu = NSNumber(value: options.getMTU())

        let ipv4 = NEIPv4Settings(
            addresses: routeAddresses(options.getInet4Address()),
            subnetMasks: routeMasks(options.getInet4Address())
        )
        ipv4.includedRoutes = routes4(options.getInet4RouteAddress(), defaultRoute: true)
        ipv4.excludedRoutes = routes4(options.getInet4RouteExcludeAddress(), defaultRoute: false)
        settings.ipv4Settings = ipv4

        let ipv6Prefixes = routePrefixes6(options.getInet6Address())
        let ipv6 = NEIPv6Settings(
            addresses: ipv6Prefixes.map(\.address),
            networkPrefixLengths: ipv6Prefixes.map { NSNumber(value: $0.prefix) }
        )
        ipv6.includedRoutes = routes6(options.getInet6RouteAddress(), defaultRoute: true)
        ipv6.excludedRoutes = routes6(options.getInet6RouteExcludeAddress(), defaultRoute: false)
        settings.ipv6Settings = ipv6

        if options.getDNSMode()?.value != LibboxDNSModeDisabled {
            let dnsIterator = try options.getDNSServerAddress()
            var servers: [String] = []
            while dnsIterator.hasNext() {
                servers.append(dnsIterator.next())
            }

            if servers.isEmpty == false {
                let dns = NEDNSSettings(servers: servers)
                dns.matchDomains = [""]
                dns.matchDomainsNoSearch = true
                settings.dnsSettings = dns
            }
        }

        return settings
    }

    private func routeAddresses(_ iterator: LibboxRoutePrefixIteratorProtocol?) -> [String] {
        routePrefixes(iterator).map(\.address)
    }

    private func routeMasks(_ iterator: LibboxRoutePrefixIteratorProtocol?) -> [String] {
        routePrefixes(iterator).map(\.mask)
    }

    private func routePrefixes(_ iterator: LibboxRoutePrefixIteratorProtocol?) -> [(address: String, prefix: Int32, mask: String)] {
        guard let iterator else {
            return []
        }

        var prefixes: [(address: String, prefix: Int32, mask: String)] = []
        while iterator.hasNext() {
            guard let prefix = iterator.next() else {
                continue
            }
            prefixes.append((prefix.address(), prefix.prefix(), prefix.mask()))
        }
        return prefixes
    }

    private func routePrefixes6(_ iterator: LibboxRoutePrefixIteratorProtocol?) -> [(address: String, prefix: Int32)] {
        routePrefixes(iterator).map { ($0.address, $0.prefix) }
    }

    private func routes4(_ iterator: LibboxRoutePrefixIteratorProtocol?, defaultRoute: Bool) -> [NEIPv4Route] {
        let prefixes = routePrefixes(iterator)
        if prefixes.isEmpty, defaultRoute {
            return [NEIPv4Route.default()]
        }
        return prefixes.map { NEIPv4Route(destinationAddress: $0.address, subnetMask: $0.mask) }
    }

    private func routes6(_ iterator: LibboxRoutePrefixIteratorProtocol?, defaultRoute: Bool) -> [NEIPv6Route] {
        let prefixes = routePrefixes6(iterator)
        if prefixes.isEmpty, defaultRoute {
            return [NEIPv6Route.default()]
        }
        return prefixes.map { NEIPv6Route(destinationAddress: $0.address, networkPrefixLength: NSNumber(value: $0.prefix)) }
    }

    func usePlatformAutoDetectControl() -> Bool { false }
    func autoDetectControl(_: Int32) throws {}
    func useProcFS() -> Bool { false }
    func underNetworkExtension() -> Bool { true }
    func includeAllNetworks() -> Bool { false }
    func clearDNSCache() {}
    func readWIFIState() -> LibboxWIFIState? { nil }
    func readWIFISSID() -> String? { nil }
    func connectSSHAgent(_ ret0_: UnsafeMutablePointer<Int32>?) throws { ret0_?.pointee = -1 }
    func localDNSTransport() -> (any LibboxLocalDNSTransportProtocol)? { nil }
    func systemCertificates() -> (any LibboxStringIteratorProtocol)? { nil }
    func usePlatformShell() -> Bool { false }
    func checkPlatformShell() throws {}
    func readSystemSSHHostKey(_ error: NSErrorPointer) -> String { "" }
    func lookupSFTPServer(_ error: NSErrorPointer) -> String { "" }
    func tailscaleHostname() -> String { "" }

    func writeLog(_ message: String?) {
        guard let message else {
            return
        }
        logger.info("\(message, privacy: .public)")
    }

    func findConnectionOwner(_: Int32, sourceAddress _: String?, sourcePort _: Int32, destinationAddress _: String?, destinationPort _: Int32) throws -> LibboxConnectionOwner {
        throw PacketTunnelProxyEngineError.engineStartFailed("Connection owner lookup is not implemented on iOS.")
    }

    func startDefaultInterfaceMonitor(_ listener: LibboxInterfaceUpdateListenerProtocol?) throws {
        guard let listener else {
            return
        }

        let monitor = NWPathMonitor()
        pathMonitor = monitor
        let semaphore = DispatchSemaphore(value: 0)
        monitor.pathUpdateHandler = { path in
            self.publishDefaultInterface(path, to: listener)
            semaphore.signal()
            monitor.pathUpdateHandler = { path in
                self.publishDefaultInterface(path, to: listener)
            }
        }
        monitor.start(queue: DispatchQueue.global(qos: .utility))
        semaphore.wait()
    }

    func closeDefaultInterfaceMonitor(_: LibboxInterfaceUpdateListenerProtocol?) throws {
        pathMonitor?.cancel()
        pathMonitor = nil
    }

    private func publishDefaultInterface(_ path: Network.NWPath, to listener: LibboxInterfaceUpdateListenerProtocol) {
        guard path.status != .unsatisfied, let defaultInterface = path.availableInterfaces.first else {
            listener.updateDefaultInterface("", interfaceIndex: -1, isExpensive: false, isConstrained: false)
            return
        }

        listener.updateDefaultInterface(
            defaultInterface.name,
            interfaceIndex: Int32(defaultInterface.index),
            isExpensive: path.isExpensive,
            isConstrained: path.isConstrained
        )
    }

    func getInterfaces() throws -> LibboxNetworkInterfaceIteratorProtocol {
        let path = pathMonitor?.currentPath
        guard let path, path.status != .unsatisfied else {
            return LibboxNetworkInterfaceArray([])
        }

        let interfaces = path.availableInterfaces.map { item in
            let networkInterface = LibboxNetworkInterface()
            networkInterface.name = item.name
            networkInterface.index = Int32(item.index)
            switch item.type {
            case .wifi:
                networkInterface.type = LibboxInterfaceTypeWIFI
            case .cellular:
                networkInterface.type = LibboxInterfaceTypeCellular
            case .wiredEthernet:
                networkInterface.type = LibboxInterfaceTypeEthernet
            default:
                networkInterface.type = LibboxInterfaceTypeOther
            }
            return networkInterface
        }

        return LibboxNetworkInterfaceArray(interfaces)
    }

    func serviceStop() throws {}
    func serviceReload() throws {}

    func getSystemProxyStatus() throws -> LibboxSystemProxyStatus {
        LibboxSystemProxyStatus()
    }

    func setSystemProxyEnabled(_: Bool) throws {}

    func triggerNativeCrash() throws {
        DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(200)) {
            fatalError("Triggered native crash for diagnostics")
        }
    }

    func writeDebugMessage(_ message: String?) {
        guard let message else {
            return
        }
        logger.debug("\(message, privacy: .public)")
    }

    func send(_: LibboxNotification?) throws {}
    func startNeighborMonitor(_: LibboxNeighborUpdateListenerProtocol?) throws {}
    func registerMyInterface(_: String?) {}
    func closeNeighborMonitor(_: LibboxNeighborUpdateListenerProtocol?) throws {}

    func openShellSession(_: LibboxPlatformUser?, command _: String?, environ _: (any LibboxStringIteratorProtocol)?, term _: String?, rows _: Int32, cols _: Int32) throws -> any LibboxShellSessionProtocol {
        throw PacketTunnelProxyEngineError.engineStartFailed("Shell sessions are not supported by OpenRung.")
    }

    func lookupUser(_ username: String?) throws -> LibboxPlatformUser {
        let user = LibboxPlatformUser()
        user.username = username ?? "mobile"
        user.uid = 501
        user.gid = 501
        user.homeDir = ""
        user.shell = ""
        return user
    }

    func reset() {
        networkSettings = nil
        pathMonitor?.cancel()
        pathMonitor = nil
    }
}

private final class LibboxNetworkInterfaceArray: NSObject, LibboxNetworkInterfaceIteratorProtocol {
    private var iterator: IndexingIterator<[LibboxNetworkInterface]>
    private var current: LibboxNetworkInterface?

    init(_ interfaces: [LibboxNetworkInterface]) {
        iterator = interfaces.makeIterator()
    }

    func hasNext() -> Bool {
        current = iterator.next()
        return current != nil
    }

    func next() -> LibboxNetworkInterface? {
        current
    }
}

private final class BlockingResultBox<T>: @unchecked Sendable {
    var result: Result<T, Error>?
}

private func runBlocking<T>(_ operation: @escaping () async throws -> T) throws -> T {
    let semaphore = DispatchSemaphore(value: 0)
    let box = BlockingResultBox<T>()

    Task {
        do {
            box.result = .success(try await operation())
        } catch {
            box.result = .failure(error)
        }
        semaphore.signal()
    }

    semaphore.wait()
    return try box.result!.get()
}
#endif
