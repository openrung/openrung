package com.openrung.client.vpn

import android.net.ConnectivityManager
import android.net.IpPrefix
import android.net.LinkProperties
import android.net.NetworkCapabilities
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import android.os.Process
import android.util.Log
import com.openrung.client.model.RelayDescriptor
import com.openrung.client.state.OpenRungStatusStore
import com.openrung.client.telemetry.TelemetryManager
import io.nekohasekai.libbox.CommandServer
import io.nekohasekai.libbox.CommandServerHandler
import io.nekohasekai.libbox.ConnectionOwner
import io.nekohasekai.libbox.InterfaceUpdateListener
import io.nekohasekai.libbox.Libbox
import io.nekohasekai.libbox.LocalDNSTransport
import io.nekohasekai.libbox.NeighborUpdateListener
import io.nekohasekai.libbox.NetworkInterfaceIterator
import io.nekohasekai.libbox.Notification
import io.nekohasekai.libbox.OverrideOptions
import io.nekohasekai.libbox.PlatformInterface
import io.nekohasekai.libbox.PlatformUser
import io.nekohasekai.libbox.RoutePrefix
import io.nekohasekai.libbox.RoutePrefixIterator
import io.nekohasekai.libbox.SetupOptions
import io.nekohasekai.libbox.ShellSession
import io.nekohasekai.libbox.StringIterator
import io.nekohasekai.libbox.SystemProxyStatus
import io.nekohasekai.libbox.TunOptions
import io.nekohasekai.libbox.WIFIState
import java.io.File
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.InterfaceAddress
import java.net.NetworkInterface as JavaNetworkInterface
import io.nekohasekai.libbox.NetworkInterface as BoxNetworkInterface

interface ProxyEngine {
    suspend fun start(
        relay: RelayDescriptor,
        configJson: String,
        vpnService: VpnService,
    )

    fun stop()
}

object ProxyEngineFactory {
    fun create(): ProxyEngine = LibboxProxyEngine()
}

class LibboxProxyEngine : ProxyEngine {
    private var commandServer: CommandServer? = null
    private var tunFd: ParcelFileDescriptor? = null

    override suspend fun start(
        relay: RelayDescriptor,
        configJson: String,
        vpnService: VpnService,
    ) {
        val platform = OpenRungLibboxPlatform(vpnService) { fd ->
            tunFd?.close()
            tunFd = fd
        }
        val handler = OpenRungCommandServerHandler(::stop)
        val options = SetupOptions().apply {
            basePath = File(vpnService.filesDir, "libbox").apply { mkdirs() }.path
            workingPath = File(vpnService.getExternalFilesDir(null) ?: vpnService.filesDir, "libbox").apply { mkdirs() }.path
            tempPath = File(vpnService.cacheDir, "libbox").apply { mkdirs() }.path
            logMaxLines = 3000
            debug = true
            crashReportSource = "OpenRungAndroid"
            oomKillerEnabled = false
            oomKillerDisabled = true
        }

        Libbox.setup(options)
        Libbox.checkConfig(configJson)

        val server = CommandServer(handler, platform)
        server.start()
        server.startOrReloadService(configJson, OverrideOptions())
        commandServer = server
    }

    override fun stop() {
        runCatching { commandServer?.closeService() }
        runCatching { commandServer?.close() }
        commandServer = null
        runCatching { tunFd?.close() }
        tunFd = null
    }
}

private class OpenRungCommandServerHandler(
    private val stopEngine: () -> Unit,
) : CommandServerHandler {
    override fun connectSSHAgent(): Int = -1

    override fun getSystemProxyStatus(): SystemProxyStatus =
        SystemProxyStatus().apply {
            available = false
            enabled = false
        }

    override fun serviceReload() = Unit

    override fun serviceStop() {
        stopEngine()
    }

    override fun setSystemProxyEnabled(isEnabled: Boolean) = Unit

    override fun triggerNativeCrash() = Unit

    override fun writeDebugMessage(message: String?) {
        message
            ?.lineSequence()
            ?.map { it.trim() }
            ?.filter { it.isNotBlank() }
            ?.forEach {
                Log.d(LOG_TAG, it)
                OpenRungStatusStore.appendLog("libbox: $it")
            }
    }
}

private class OpenRungLibboxPlatform(
    private val vpnService: VpnService,
    private val onTunOpened: (ParcelFileDescriptor) -> Unit,
) : PlatformInterface {
    override fun usePlatformAutoDetectInterfaceControl(): Boolean = true

    override fun autoDetectInterfaceControl(fd: Int) {
        vpnService.protect(fd)
    }

    override fun openTun(options: TunOptions): Int {
        check(VpnService.prepare(vpnService) == null) { "android: missing VPN permission" }

        val builder = vpnService.Builder()
            .setSession("OpenRung")
            .setMtu(options.mtu)

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            builder.setMetered(false)
        }

        options.inet4Address.forEachRemaining { builder.addAddress(it.address(), it.prefix()) }
        options.inet6Address.forEachRemaining { builder.addAddress(it.address(), it.prefix()) }

        if (options.autoRoute) {
            if (options.dnsMode.value != Libbox.DNSModeDisabled) {
                options.dnsServerAddress.forEachRemaining { builder.addDnsServer(it) }
            }

            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                var addedRoute = false
                options.inet4RouteAddress.forEachRemaining {
                    builder.addRoute(it.toIpPrefix())
                    addedRoute = true
                }
                if (!addedRoute && options.inet4Address.hasNext()) {
                    builder.addRoute("0.0.0.0", 0)
                }

                addedRoute = false
                options.inet6RouteAddress.forEachRemaining {
                    builder.addRoute(it.toIpPrefix())
                    addedRoute = true
                }
                if (!addedRoute && options.inet6Address.hasNext()) {
                    builder.addRoute("::", 0)
                }

                options.inet4RouteExcludeAddress.forEachRemaining { builder.excludeRoute(it.toIpPrefix()) }
                options.inet6RouteExcludeAddress.forEachRemaining { builder.excludeRoute(it.toIpPrefix()) }
            } else {
                options.inet4RouteRange.forEachRemaining { builder.addRoute(it.address(), it.prefix()) }
                options.inet6RouteRange.forEachRemaining { builder.addRoute(it.address(), it.prefix()) }
            }

            options.includePackage.forEachRemaining { builder.addAllowedApplication(it) }
            options.excludePackage.forEachRemaining { builder.addDisallowedApplication(it) }
        }

        val fd = builder.establish() ?: error("android: the VPN tunnel could not be established")
        onTunOpened(fd)
        return fd.fd
    }

    override fun useProcFS(): Boolean = Build.VERSION.SDK_INT < Build.VERSION_CODES.Q

    override fun findConnectionOwner(
        ipProtocol: Int,
        sourceAddress: String?,
        sourcePort: Int,
        destinationAddress: String?,
        destinationPort: Int,
    ): ConnectionOwner {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return ConnectionOwner()
        if (sourceAddress.isNullOrBlank() || destinationAddress.isNullOrBlank()) return ConnectionOwner()

        return runCatching {
            val connectivityManager = vpnService.getSystemService(ConnectivityManager::class.java)
            val uid = connectivityManager.getConnectionOwnerUid(
                ipProtocol,
                sourceAddress.toSocketAddress(sourcePort),
                destinationAddress.toSocketAddress(destinationPort),
            )
            if (uid == Process.INVALID_UID) return@runCatching ConnectionOwner()

            val packages = vpnService.packageManager.getPackagesForUid(uid)?.toList().orEmpty()
            TelemetryManager.recordApplicationConnection(
                uid = uid,
                packages = packages,
                destinationIp = destinationAddress,
                destinationPort = destinationPort,
                ipProtocol = ipProtocol,
            )
            ConnectionOwner().apply {
                userId = uid
                userName = uid.toString()
                processPath = packages.firstOrNull().orEmpty()
                setAndroidPackageNames(ListStringIterator(packages))
            }
        }.onFailure {
            Log.w(LOG_TAG, "could not identify Android connection owner", it)
        }.getOrElse { ConnectionOwner() }
    }

    override fun getInterfaces(): NetworkInterfaceIterator =
        ListNetworkInterfaceIterator(discoverInterfaces())

    override fun underNetworkExtension(): Boolean = false

    override fun includeAllNetworks(): Boolean = false

    override fun clearDNSCache() = Unit

    override fun readWIFIState(): WIFIState? = null

    override fun localDNSTransport(): LocalDNSTransport? = null

    override fun systemCertificates(): StringIterator = EmptyStringIterator

    override fun startDefaultInterfaceMonitor(listener: InterfaceUpdateListener?) {
        updateDefaultInterface(listener)
    }

    override fun closeDefaultInterfaceMonitor(listener: InterfaceUpdateListener?) = Unit

    override fun startNeighborMonitor(listener: NeighborUpdateListener?) = Unit

    override fun closeNeighborMonitor(listener: NeighborUpdateListener?) = Unit

    override fun usePlatformShell(): Boolean = false

    override fun checkPlatformShell() = Unit

    override fun openShellSession(
        user: PlatformUser?,
        command: String?,
        environ: StringIterator?,
        term: String?,
        rows: Int,
        cols: Int,
    ): ShellSession {
        throw UnsupportedOperationException("platform shell is not supported")
    }

    override fun readSystemSSHHostKey(): String = ""

    override fun lookupSFTPServer(): String = ""

    override fun tailscaleHostname(): String = ""

    override fun lookupUser(username: String?): PlatformUser {
        throw UnsupportedOperationException("user lookup is not supported")
    }

    override fun registerMyInterface(name: String?) = Unit

    override fun sendNotification(notification: Notification?) = Unit

    private fun discoverInterfaces(): List<BoxNetworkInterface> {
        val androidNetworks = discoverAndroidNetworks().associateBy { it.interfaceName }

        val javaInterfaces = JavaNetworkInterface.getNetworkInterfaces()?.toList().orEmpty()
        val orderedNames = androidNetworks.keys + javaInterfaces.map { it.name }

        return orderedNames
            .distinct()
            .mapNotNull { name ->
                val javaInterface = javaInterfaces.firstOrNull { it.name == name } ?: return@mapNotNull null
                if (!javaInterface.isUsableUnderlyingInterface()) return@mapNotNull null
                javaInterface.toBoxNetworkInterface(androidNetworks[name])
            }
    }

    private fun updateDefaultInterface(listener: InterfaceUpdateListener?) {
        if (listener == null) return

        val defaultNetwork = defaultAndroidNetwork()
        val defaultInterface = defaultNetwork?.let { JavaNetworkInterface.getByName(it.interfaceName) }
        if (defaultNetwork == null || defaultInterface == null || !defaultInterface.isUsableUnderlyingInterface()) {
            Log.w(LOG_TAG, "no Android default network interface available for libbox")
            listener.updateDefaultInterface("", -1, false, false)
            return
        }

        Log.d(
            LOG_TAG,
            "default network for libbox: ${defaultNetwork.interfaceName}#${defaultInterface.index}",
        )
        listener.updateDefaultInterface(
            defaultNetwork.interfaceName,
            defaultInterface.index,
            defaultNetwork.isMetered,
            defaultNetwork.isConstrained,
        )
    }

    private fun defaultAndroidNetwork(): AndroidNetworkInfo? {
        val connectivityManager = vpnService.getSystemService(ConnectivityManager::class.java)
        val activeNetwork = connectivityManager.activeNetwork
        val networks = discoverAndroidNetworks(connectivityManager)
        return networks.firstOrNull { it.network == activeNetwork }
            ?: networks.firstOrNull { it.capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) }
            ?: networks.firstOrNull()
    }

    private fun discoverAndroidNetworks(
        connectivityManager: ConnectivityManager = vpnService.getSystemService(ConnectivityManager::class.java),
    ): List<AndroidNetworkInfo> =
        connectivityManager.allNetworks.mapNotNull { network ->
            val capabilities = connectivityManager.getNetworkCapabilities(network) ?: return@mapNotNull null
            if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) return@mapNotNull null

            val linkProperties = connectivityManager.getLinkProperties(network) ?: return@mapNotNull null
            val interfaceName = linkProperties.interfaceName ?: return@mapNotNull null
            AndroidNetworkInfo(
                network = network,
                interfaceName = interfaceName,
                capabilities = capabilities,
                linkProperties = linkProperties,
            )
        }
}

private object EmptyStringIterator : StringIterator {
    override fun len(): Int = 0
    override fun hasNext(): Boolean = false
    override fun next(): String = ""
}

private object EmptyNetworkInterfaceIterator : NetworkInterfaceIterator {
    override fun hasNext(): Boolean = false
    override fun next(): BoxNetworkInterface =
        throw NoSuchElementException()
}

private class ListStringIterator(private val values: List<String>) : StringIterator {
    private val iterator = values.iterator()

    override fun len(): Int = values.size
    override fun hasNext(): Boolean = iterator.hasNext()
    override fun next(): String = iterator.next()
}

private class ListNetworkInterfaceIterator(values: List<BoxNetworkInterface>) : NetworkInterfaceIterator {
    private val iterator = values.iterator()

    override fun hasNext(): Boolean = iterator.hasNext()
    override fun next(): BoxNetworkInterface = iterator.next()
}

private fun StringIterator.forEachRemaining(block: (String) -> Unit) {
    while (hasNext()) block(next())
}

private fun String.toSocketAddress(port: Int): InetSocketAddress {
    val normalized = trim().removePrefix("[").substringBefore("]").substringBefore("%")
    return InetSocketAddress(InetAddress.getByName(normalized), port)
}

private fun RoutePrefixIterator.forEachRemaining(block: (RoutePrefix) -> Unit) {
    while (hasNext()) block(next())
}

private fun RoutePrefix.toIpPrefix(): IpPrefix =
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
        IpPrefix(InetAddress.getByName(address()), prefix())
    } else {
        error("IpPrefix routes require Android 13 or newer")
    }

private data class AndroidNetworkInfo(
    val network: android.net.Network,
    val interfaceName: String,
    val capabilities: NetworkCapabilities,
    val linkProperties: LinkProperties,
) {
    val isMetered: Boolean
        get() = !capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)

    val isConstrained: Boolean
        get() = !capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
}

private fun JavaNetworkInterface.isUsableUnderlyingInterface(): Boolean {
    val name = name
    return runCatching {
        isUp &&
            !isLoopback &&
            !name.startsWith("tun") &&
            !name.startsWith("lo") &&
            !name.startsWith("ip6tnl") &&
            !name.startsWith("sit")
    }.getOrDefault(false)
}

private fun JavaNetworkInterface.toBoxNetworkInterface(info: AndroidNetworkInfo?): BoxNetworkInterface =
    BoxNetworkInterface().apply {
        index = this@toBoxNetworkInterface.index
        mtu = this@toBoxNetworkInterface.mtu
        name = this@toBoxNetworkInterface.name
        addresses = ListStringIterator(interfaceAddresses.mapNotNull { it.toPrefixString() })
        flags = networkFlags()
        type = info?.capabilities?.interfaceType() ?: Libbox.InterfaceTypeOther
        dnsServer = ListStringIterator(
            info?.linkProperties
                ?.dnsServers
                ?.mapNotNull { it.hostAddress?.withoutZoneId() }
                .orEmpty(),
        )
        metered = info?.isMetered == true
    }

private fun InterfaceAddress.toPrefixString(): String? {
    val address = address?.hostAddress?.withoutZoneId() ?: return null
    val prefixLength = networkPrefixLength.toInt()
    val maxPrefixLength = if (address.contains(":")) 128 else 32
    if (prefixLength !in 0..maxPrefixLength) return null
    return "$address/$prefixLength"
}

private fun String.withoutZoneId(): String = substringBefore('%')

private fun JavaNetworkInterface.networkFlags(): Int {
    var flags = 0
    if (runCatching { isUp }.getOrDefault(false)) flags = flags or NET_FLAG_UP
    if (runCatching { !isLoopback && interfaceAddresses.any { it.broadcast != null } }.getOrDefault(false)) {
        flags = flags or NET_FLAG_BROADCAST
    }
    if (runCatching { isLoopback }.getOrDefault(false)) flags = flags or NET_FLAG_LOOPBACK
    if (runCatching { isPointToPoint }.getOrDefault(false)) flags = flags or NET_FLAG_POINT_TO_POINT
    if (runCatching { supportsMulticast() }.getOrDefault(false)) flags = flags or NET_FLAG_MULTICAST
    return flags
}

private fun NetworkCapabilities.interfaceType(): Int =
    when {
        hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> Libbox.InterfaceTypeWIFI
        hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> Libbox.InterfaceTypeCellular
        hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> Libbox.InterfaceTypeEthernet
        else -> Libbox.InterfaceTypeOther
    }

private const val LOG_TAG = "OpenRungLibbox"
private const val NET_FLAG_UP = 1
private const val NET_FLAG_BROADCAST = 2
private const val NET_FLAG_LOOPBACK = 4
private const val NET_FLAG_POINT_TO_POINT = 8
private const val NET_FLAG_MULTICAST = 16
