package com.openrung.client.vpn

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.SystemClock
import androidx.core.app.NotificationCompat
import com.openrung.client.MainActivity
import com.openrung.client.R
import com.openrung.client.config.AppConfig
import com.openrung.client.model.CountryGeo
import com.openrung.client.model.RecentNode
import com.openrung.client.model.RelayDescriptor
import com.openrung.client.model.RelaySelector
import com.openrung.client.net.BrokerClient
import com.openrung.client.net.ClientGeoInfo
import com.openrung.client.net.GeoIpClient
import com.openrung.client.net.InternetProbe
import com.openrung.client.net.RelayReachability
import com.openrung.client.net.SingBoxConfiguration
import com.openrung.client.state.ConnectionStatus
import com.openrung.client.state.OpenRungStatusStore
import com.openrung.client.telemetry.TelemetryManager
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlin.random.Random

class OpenRungVpnService : VpnService() {
    private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val relaySelector = RelaySelector()
    private var connectJob: Job? = null
    private var heartbeatJob: Job? = null
    private var engine: ProxyEngine? = null
    private var brokerUrl: String = AppConfig.DEFAULT_BROKER_URL
    private var activeRelayId: String? = null

    override fun onCreate() {
        super.onCreate()
        OpenRungStatusStore.initialize(applicationContext)
        TelemetryManager.initialize(applicationContext)
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_CONNECT -> {
                val brokerUrl = intent.getStringExtra(EXTRA_BROKER_URL).orEmpty()
                val targetCountry = intent.getStringExtra(EXTRA_TARGET_COUNTRY)?.takeIf { it.isNotBlank() }
                heartbeatJob?.cancel()
                connectJob?.cancel()
                connectJob = serviceScope.launch {
                    connect(brokerUrl.ifBlank { AppConfig.DEFAULT_BROKER_URL }, targetCountry)
                }
            }
            ACTION_DISCONNECT -> disconnect()
        }
        return START_STICKY
    }

    override fun onRevoke() {
        disconnect()
        super.onRevoke()
    }

    override fun onDestroy() {
        disconnect()
        connectJob?.cancel()
        super.onDestroy()
    }

    private suspend fun connect(brokerUrl: String, targetCountry: String? = null) {
        this.brokerUrl = brokerUrl
        // Tear down any existing tunnel first so tapping a different location cleanly switches relays.
        cleanupActiveTunnel()
        // Telemetry/heartbeat go DIRECT to the origin IP, not the Cloudflare-fronted discovery broker,
        // so high-frequency heartbeats don't burn the Workers free-tier quota (see AppConfig).
        val telemetrySession = TelemetryManager.beginSession(applicationContext, AppConfig.TELEMETRY_BROKER_URL)
        var failureStage = "preparing"
        TelemetryManager.record("connection_attempted")
        OpenRungStatusStore.setBrokerUrl(brokerUrl)
        OpenRungStatusStore.clearError()
        OpenRungStatusStore.setStatus(ConnectionStatus.PREPARING, relayLabel = null, lastError = null)
        startForeground(NOTIFICATION_ID, notification(getString(R.string.vpn_notification_preparing)))

        try {
            OpenRungStatusStore.setStatus(ConnectionStatus.CONNECTING)
            OpenRungStatusStore.appendLog(getString(R.string.log_fetching_relays, brokerUrl))
            failureStage = "broker_fetch"
            val brokerEndpoints = AppConfig.brokerCandidates(brokerUrl)
            val (fetch, brokerFetchMs) = coroutineScope {
                val geoLookup = async {
                    runCatching { GeoIpClient().lookup() }.getOrNull()
                }
                val brokerStarted = SystemClock.elapsedRealtime()
                // When targeting a specific country, fetch the full relay set so that country's
                // relays are present (the default page may otherwise miss them). Tries each broker
                // candidate in order so a blocked primary endpoint doesn't take discovery offline.
                val result = BrokerClient.firstReachable(
                    candidates = brokerEndpoints,
                    limit = if (targetCountry != null) AppConfig.DIRECTORY_RELAY_LIMIT else AppConfig.RELAY_LIMIT,
                    clientId = telemetrySession.clientId,
                    sessionId = telemetrySession.id,
                )
                val elapsed = SystemClock.elapsedRealtime() - brokerStarted
                geoLookup.await()?.let(TelemetryManager::setGeoInfo)
                result to elapsed
            }
            val relayResponse = fetch.response
            // If the configured/primary broker was unreachable and a fallback served the list, pin the
            // rest of this session's broker traffic (telemetry, heartbeats) to the endpoint that worked.
            // The persisted/configured broker URL is left untouched so a user's custom choice survives.
            if (fetch.brokerUrl != brokerUrl) {
                this.brokerUrl = fetch.brokerUrl
                OpenRungStatusStore.appendLog(getString(R.string.log_broker_fallback, fetch.brokerUrl))
            }
            val candidates = relaySelector.orderedCandidates(relayResponse.relays, relayResponse.serverInstant)
            OpenRungStatusStore.appendLog(
                getString(R.string.log_broker_returned, relayResponse.relays.size, candidates.size),
            )
            check(candidates.isNotEmpty()) { getString(R.string.error_no_usable_relay) }

            val targetedCandidates = if (targetCountry != null) {
                val countryName = CountryGeo.displayName(targetCountry) ?: targetCountry
                OpenRungStatusStore.appendLog(getString(R.string.log_connecting_country, countryName))
                failureStage = "relay_geo_filter"
                filterByCountry(candidates, targetCountry).also {
                    check(it.isNotEmpty()) { getString(R.string.error_no_relay_in_country, countryName) }
                }
            } else {
                candidates
            }

            failureStage = "relay_connect"
            val connectedRelay = connectFirstAvailable(targetedCandidates)
            val relay = connectedRelay.relay
            activeRelayId = relay.id
            TelemetryManager.markConnected(relay.id)
            OpenRungStatusStore.setStatus(
                ConnectionStatus.CONNECTED,
                relayLabel = null,
                lastError = null,
            )
            updateNotification(getString(R.string.status_connected))
            resolveRelayLocation(relay)
            TelemetryManager.record(
                event = "connection_succeeded",
                relayId = relay.id,
                measurements = mapOf(
                    "broker_fetch_ms" to brokerFetchMs,
                    "relay_tcp_ms" to connectedRelay.tcpLatencyMs,
                    "tunnel_start_ms" to connectedRelay.tunnelStartMs,
                    "internet_probe_ms" to connectedRelay.internetProbeMs,
                    "relay_attempts" to connectedRelay.attempts.toLong(),
                ),
            )
            runCatching { TelemetryManager.flush(AppConfig.TELEMETRY_BROKER_URL) }
            startHeartbeatLoop()
        } catch (error: CancellationException) {
            throw error
        } catch (error: Throwable) {
            cleanupActiveTunnel()
            TelemetryManager.record(
                event = "connection_failed",
                attributes = mapOf(
                    "failure_stage" to failureStage,
                    "error_type" to error::class.java.simpleName,
                ),
            )
            TelemetryManager.endSession("connection_failed")
            runCatching { TelemetryManager.flush(AppConfig.TELEMETRY_BROKER_URL) }
            OpenRungStatusStore.fail(error.message ?: getString(R.string.error_vpn_connection_failed))
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
        }
    }

    private suspend fun connectFirstAvailable(candidates: List<RelayDescriptor>): ConnectedRelay {
        var lastError: Throwable? = null
        for ((index, relay) in candidates.withIndex()) {
            try {
                OpenRungStatusStore.appendLog(
                    getString(R.string.log_trying_relay, relay.id, relay.publicHost, relay.publicPort),
                )
                OpenRungStatusStore.appendLog(getString(R.string.log_checking_relay_reachability))
                val tcpLatencyMs = try {
                    RelayReachability.checkTcp(relay)
                } catch (error: Throwable) {
                    throw IllegalStateException(
                        getString(R.string.error_relay_unreachable, relay.publicHost, relay.publicPort),
                        error,
                    )
                }
                val config = SingBoxConfiguration(relay = relay).encodedJsonString()
                val proxyEngine = ProxyEngineFactory.create()
                val tunnelStarted = SystemClock.elapsedRealtime()
                proxyEngine.start(
                    relay = relay,
                    configJson = config,
                    vpnService = this,
                )
                val tunnelStartMs = SystemClock.elapsedRealtime() - tunnelStarted
                engine = proxyEngine
                OpenRungStatusStore.appendLog(getString(R.string.log_verifying_internet))
                val internetProbe = InternetProbe(applicationContext).verify()
                OpenRungStatusStore.appendLog(
                    getString(R.string.log_internet_verified, internetProbe.durationMs),
                )
                return ConnectedRelay(
                    relay = relay,
                    tcpLatencyMs = tcpLatencyMs,
                    tunnelStartMs = tunnelStartMs,
                    internetProbeMs = internetProbe.durationMs,
                    attempts = index + 1,
                )
            } catch (error: CancellationException) {
                throw error
            } catch (error: Throwable) {
                lastError = error
                TelemetryManager.record(
                    event = "relay_attempt_failed",
                    relayId = relay.id,
                    attributes = mapOf("error_type" to error::class.java.simpleName),
                    measurements = mapOf("attempt" to (index + 1).toLong()),
                )
                OpenRungStatusStore.appendLog(
                    getString(
                        R.string.log_relay_failed,
                        relay.id,
                        error.message ?: error::class.java.simpleName,
                    ),
                )
                cleanupActiveTunnel()
            }
        }

        throw IllegalStateException(
            getString(
                R.string.error_all_relays_failed,
                lastError?.message ?: getString(R.string.error_unknown),
            ),
        )
    }

    /**
     * Resolves each candidate relay's country via GeoIP (concurrently, deduped by host) and keeps
     * only those in [countryCode]. Relays whose geo can't be resolved are excluded so a targeted
     * connect never silently lands in the wrong country.
     */
    private suspend fun filterByCountry(
        candidates: List<RelayDescriptor>,
        countryCode: String,
    ): List<RelayDescriptor> = coroutineScope {
        val target = countryCode.trim().uppercase()
        val countryByHost = candidates.map { it.publicHost }.distinct()
            .map { host ->
                async {
                    host to runCatching { GeoIpClient().lookup(host).countryCode.trim().uppercase() }.getOrNull()
                }
            }
            .awaitAll()
            .toMap()
        candidates.filter { countryByHost[it.publicHost] == target }
    }

    private fun disconnect() {
        heartbeatJob?.cancel()
        heartbeatJob = null
        OpenRungStatusStore.setStatus(ConnectionStatus.DISCONNECTING)
        connectJob?.cancel()
        cleanupActiveTunnel()
        activeRelayId?.let {
            TelemetryManager.record("tunnel_stopped", relayId = it)
        }
        activeRelayId = null
        TelemetryManager.endSession("disconnect")
        serviceScope.launch {
            runCatching { TelemetryManager.flush(AppConfig.TELEMETRY_BROKER_URL) }
        }
        stopForeground(STOP_FOREGROUND_REMOVE)
        OpenRungStatusStore.setStatus(ConnectionStatus.DISCONNECTED, relayLabel = null, lastError = null)
        stopSelf()
    }

    /**
     * Resolves the relay's geographic location off the main path and shows only that location
     * (never the raw IP). The relay line stays hidden until resolved; on failure it falls back to
     * a generic label. Guarded by [activeRelayId] so a late result can't overwrite a disconnect.
     */
    private fun resolveRelayLocation(relay: RelayDescriptor) {
        serviceScope.launch {
            val geo = runCatching { GeoIpClient().lookup(relay.publicHost) }.getOrNull()
            val location = geo?.locationLabel()?.takeIf { it.isNotBlank() }
                ?: getString(R.string.relay_location_unknown)
            if (activeRelayId != relay.id) return@launch
            OpenRungStatusStore.setRelayLabel(location)
            updateNotification(getString(R.string.vpn_notification_connected, location))
            geo?.let(::recordRecentNode)
        }
    }

    /** Adds the connected relay's country to the main-screen "Recents" row (best-effort). */
    private fun recordRecentNode(geo: ClientGeoInfo) {
        val code = geo.countryCode.trim().uppercase()
        if (code.isBlank()) return
        val centroid = CountryGeo.centroid(code)
        OpenRungStatusStore.recordRecent(
            RecentNode(
                countryCode = code,
                label = geo.locationLabel().ifBlank { centroid?.name ?: code },
                latitude = centroid?.latitude ?: geo.latitude,
                longitude = centroid?.longitude ?: geo.longitude,
            ),
        )
    }

    private fun startHeartbeatLoop() {
        heartbeatJob?.cancel()
        heartbeatJob = serviceScope.launch {
            while (isActive) {
                runCatching { TelemetryManager.sendHeartbeat() }
                delay(Random.nextLong(HEARTBEAT_MIN_DELAY_MS, HEARTBEAT_MAX_DELAY_MS + 1))
            }
        }
    }

    private fun cleanupActiveTunnel() {
        engine?.stop()
        engine = null
    }

    private data class ConnectedRelay(
        val relay: RelayDescriptor,
        val tcpLatencyMs: Long,
        val tunnelStartMs: Long,
        val internetProbeMs: Long,
        val attempts: Int,
    )

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val manager = getSystemService(NotificationManager::class.java)
        val channel = NotificationChannel(
            NOTIFICATION_CHANNEL_ID,
            getString(R.string.vpn_notification_channel),
            NotificationManager.IMPORTANCE_LOW,
        )
        manager.createNotificationChannel(channel)
    }

    private fun updateNotification(message: String) {
        val manager = getSystemService(NotificationManager::class.java)
        manager.notify(NOTIFICATION_ID, notification(message))
    }

    private fun notification(message: String): Notification {
        val launchIntent = Intent(this, MainActivity::class.java)
        val pendingIntent = PendingIntent.getActivity(
            this,
            0,
            launchIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return NotificationCompat.Builder(this, NOTIFICATION_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_vpn)
            .setContentTitle(getString(R.string.vpn_notification_title))
            .setContentText(message)
            .setContentIntent(pendingIntent)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .build()
    }

    companion object {
        private const val ACTION_CONNECT = "com.openrung.client.action.CONNECT"
        private const val ACTION_DISCONNECT = "com.openrung.client.action.DISCONNECT"
        private const val EXTRA_BROKER_URL = "broker_url"
        private const val EXTRA_TARGET_COUNTRY = "target_country"
        private const val NOTIFICATION_CHANNEL_ID = "openrung_vpn"
        private const val NOTIFICATION_ID = 2001
        internal const val HEARTBEAT_MIN_DELAY_MS = 50_000L
        internal const val HEARTBEAT_MAX_DELAY_MS = 70_000L

        fun connectIntent(context: Context, brokerUrl: String, targetCountry: String? = null): Intent =
            Intent(context, OpenRungVpnService::class.java).apply {
                action = ACTION_CONNECT
                putExtra(EXTRA_BROKER_URL, brokerUrl)
                targetCountry?.let { putExtra(EXTRA_TARGET_COUNTRY, it) }
            }

        fun disconnectIntent(context: Context): Intent =
            Intent(context, OpenRungVpnService::class.java).apply {
                action = ACTION_DISCONNECT
            }
    }
}
