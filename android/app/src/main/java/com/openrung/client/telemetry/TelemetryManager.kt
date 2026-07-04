package com.openrung.client.telemetry

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import android.os.SystemClock
import com.openrung.client.BuildConfig
import com.openrung.client.net.ClientGeoInfo
import com.openrung.client.net.SpeedTestResult
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.time.Instant
import java.util.Locale
import java.util.TimeZone
import java.util.UUID

object TelemetryManager {
    private const val PREFS = "openrung_telemetry"
    private const val KEY_OUTBOX = "outbox"
    private const val MAX_QUEUED_EVENTS = 500
    private const val UPLOAD_BATCH_SIZE = 200

    private val lock = Any()
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    private var context: Context? = null
    private var activeSession: Session? = null

    data class Session(
        val id: String,
        val clientId: String,
        val brokerUrl: String,
        val startedElapsedMs: Long,
        val relayId: String? = null,
        val connectedElapsedMs: Long? = null,
        val geoAttributes: Map<String, String> = emptyMap(),
    )

    fun initialize(context: Context) {
        synchronized(lock) {
            if (this.context == null) this.context = context.applicationContext
        }
    }

    fun clientId(context: Context): String = ClientIdentity.getOrCreate(context.applicationContext)

    fun beginSession(context: Context, brokerUrl: String): Session {
        initialize(context)
        return Session(
            id = UUID.randomUUID().toString(),
            clientId = clientId(context),
            brokerUrl = brokerUrl,
            startedElapsedMs = SystemClock.elapsedRealtime(),
        ).also { synchronized(lock) { activeSession = it } }
    }

    fun activeSession(): Session? = synchronized(lock) { activeSession }

    fun markConnected(relayId: String) {
        synchronized(lock) {
            activeSession = activeSession?.copy(
                relayId = relayId,
                connectedElapsedMs = SystemClock.elapsedRealtime(),
            )
        }
    }

    fun setGeoInfo(geoInfo: ClientGeoInfo) {
        val appContext = context ?: return
        val geoAttributes = geoInfo.telemetryAttributes()
        synchronized(lock) {
            val session = activeSession ?: return
            activeSession = session.copy(geoAttributes = geoAttributes)
            writeOutbox(
                appContext,
                readOutbox(appContext).map { event ->
                    if (event.sessionId == session.id) {
                        event.copy(attributes = event.attributes + geoAttributes)
                    } else {
                        event
                    }
                },
            )
        }
        record("client_geo_resolved")
    }

    fun record(
        event: String,
        relayId: String? = null,
        attributes: Map<String, String> = emptyMap(),
        measurements: Map<String, Long> = emptyMap(),
    ) {
        val appContext = context ?: return
        val session = activeSession() ?: return
        enqueue(
            appContext,
            TelemetryEvent(
                eventId = UUID.randomUUID().toString(),
                event = event,
                occurredAt = Instant.now().toString(),
                clientId = session.clientId,
                sessionId = session.id,
                relayId = relayId ?: session.relayId,
                attributes = deviceAttributes(appContext) + session.geoAttributes + attributes,
                measurements = measurements,
            ),
        )
    }

    fun recordApplicationConnection(
        uid: Int,
        packages: List<String>,
        destinationIp: String?,
        destinationPort: Int,
        ipProtocol: Int,
    ) {
        val appContext = context ?: return
        val session = activeSession() ?: return
        val externalPackages = packages.filterNot { it == appContext.packageName }
        if (externalPackages.isEmpty()) return

        externalPackages.forEach { packageName ->
            enqueue(
                appContext,
                TelemetryEvent(
                    eventId = UUID.randomUUID().toString(),
                    event = "application_connection",
                    occurredAt = Instant.now().toString(),
                    clientId = session.clientId,
                    sessionId = session.id,
                    relayId = session.relayId,
                    applicationPackage = packageName,
                    applicationUid = uid,
                    destinationIp = destinationIp,
                    destinationPort = destinationPort,
                    protocol = protocolName(ipProtocol),
                    attributes = session.geoAttributes,
                ),
            )
        }
    }

    fun endSession(reason: String) {
        val session = activeSession() ?: return
        val now = SystemClock.elapsedRealtime()
        val measurements = mutableMapOf("session_duration_ms" to (now - session.startedElapsedMs))
        session.connectedElapsedMs?.let { measurements["connection_duration_ms"] = now - it }
        record(
            event = "connection_ended",
            relayId = session.relayId,
            attributes = mapOf("reason" to reason),
            measurements = measurements,
        )
        synchronized(lock) { if (activeSession?.id == session.id) activeSession = null }
    }

    fun recordSpeedTest(result: SpeedTestResult) {
        record(
            event = "speed_test_completed",
            attributes = mapOf(
                "provider" to "openrung_broker",
                "test_type" to "manual_download",
            ),
            measurements = mapOf(
                "bytes_downloaded" to result.bytesDownloaded,
                "download_duration_ms" to result.durationMs,
                "time_to_first_byte_ms" to result.timeToFirstByteMs,
                "download_mbps_milli" to (result.downloadMbps * 1_000.0).toLong(),
            ),
        )
    }

    suspend fun sendHeartbeat() {
        val appContext = context ?: return
        val session = activeSession() ?: return
        val event = buildSessionHeartbeat(
            session = session,
            occurredAt = Instant.now(),
            elapsedRealtimeMs = SystemClock.elapsedRealtime(),
            attributes = deviceAttributes(appContext) + session.geoAttributes,
        ) ?: return
        val queued = synchronized(lock) { readOutbox(appContext).take(UPLOAD_BATCH_SIZE - 1) }
        TelemetryClient(session.brokerUrl).send(queued + event)
        if (queued.isNotEmpty()) {
            synchronized(lock) {
                val sentIDs = queued.mapTo(hashSetOf()) { it.eventId }
                writeOutbox(appContext, readOutbox(appContext).filterNot { it.eventId in sentIDs })
            }
            flush(session.brokerUrl)
        }
    }

    suspend fun flush(brokerUrl: String) {
        val appContext = context ?: return
        while (true) {
            val batch = synchronized(lock) { readOutbox(appContext).take(UPLOAD_BATCH_SIZE) }
            if (batch.isEmpty()) return
            TelemetryClient(brokerUrl).send(batch)
            synchronized(lock) {
                val sentIDs = batch.mapTo(hashSetOf()) { it.eventId }
                writeOutbox(appContext, readOutbox(appContext).filterNot { it.eventId in sentIDs })
            }
        }
    }

    private fun enqueue(context: Context, event: TelemetryEvent) {
        synchronized(lock) {
            writeOutbox(context, (readOutbox(context) + event).takeLast(MAX_QUEUED_EVENTS))
        }
    }

    private fun readOutbox(context: Context): List<TelemetryEvent> {
        val encoded = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_OUTBOX, null)
            ?: return emptyList()
        return runCatching { json.decodeFromString<List<TelemetryEvent>>(encoded) }.getOrDefault(emptyList())
    }

    private fun writeOutbox(context: Context, events: List<TelemetryEvent>) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit()
            .putString(KEY_OUTBOX, json.encodeToString(events))
            .apply()
    }

    private fun deviceAttributes(context: Context): Map<String, String> {
        val connectivity = context.getSystemService(ConnectivityManager::class.java)
        val capabilities = connectivity.getNetworkCapabilities(connectivity.activeNetwork)
        return mapOf(
            "app_version" to BuildConfig.VERSION_NAME,
            "android_api" to Build.VERSION.SDK_INT.toString(),
            "device_manufacturer" to Build.MANUFACTURER,
            "device_model" to Build.MODEL,
            "locale" to Locale.getDefault().toLanguageTag(),
            "timezone" to TimeZone.getDefault().id,
            "network_transport" to transportName(capabilities),
            "network_metered" to (capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED) != true).toString(),
            "network_roaming" to (capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_ROAMING) != true).toString(),
        )
    }

    private fun transportName(capabilities: NetworkCapabilities?): String = when {
        capabilities == null -> "unknown"
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
        else -> "other"
    }

    private fun protocolName(protocol: Int): String = when (protocol) {
        6 -> "tcp"
        17 -> "udp"
        else -> protocol.toString()
    }
}

internal fun buildSessionHeartbeat(
    session: TelemetryManager.Session,
    occurredAt: Instant,
    elapsedRealtimeMs: Long,
    attributes: Map<String, String>,
): TelemetryEvent? {
    val relayId = session.relayId ?: return null
    val connectedElapsedMs = session.connectedElapsedMs ?: return null
    return TelemetryEvent(
        eventId = UUID.randomUUID().toString(),
        event = "session_heartbeat",
        occurredAt = occurredAt.toString(),
        clientId = session.clientId,
        sessionId = session.id,
        relayId = relayId,
        attributes = attributes + ("connection_state" to "connected"),
        measurements = mapOf(
            "session_duration_ms" to (elapsedRealtimeMs - session.startedElapsedMs).coerceAtLeast(0),
            "connected_duration_ms" to (elapsedRealtimeMs - connectedElapsedMs).coerceAtLeast(0),
        ),
    )
}
