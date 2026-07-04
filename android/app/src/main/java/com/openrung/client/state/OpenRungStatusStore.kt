package com.openrung.client.state

import android.content.Context
import com.openrung.client.config.AppConfig
import com.openrung.client.model.RecentNode
import com.openrung.client.net.BrokerClient
import com.openrung.client.net.ExitNodeDirectory
import com.openrung.client.net.GeoIpClient
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json
import java.time.LocalTime
import java.time.format.DateTimeFormatter

object OpenRungStatusStore {
    private const val KEY_STATUS = "status"
    private const val KEY_BROKER_URL = "broker_url"
    private const val KEY_RELAY_LABEL = "relay_label"
    private const val KEY_LAST_ERROR = "last_error"
    private const val KEY_LOG_LINES = "log_lines"
    private const val KEY_RECENT_NODES = "recent_nodes"
    private const val MAX_LOG_LINES = 80

    private val timeFormatter = DateTimeFormatter.ofPattern("HH:mm:ss")
    private val json = Json { ignoreUnknownKeys = true }
    private val state = MutableStateFlow(OpenRungUiState(brokerUrl = AppConfig.DEFAULT_BROKER_URL))
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private var appContext: Context? = null
    private var directoryJob: Job? = null

    val uiState: StateFlow<OpenRungUiState> = state.asStateFlow()

    fun initialize(context: Context) {
        if (appContext != null) return
        appContext = context.applicationContext
        val prefs = context.getSharedPreferences(AppConfig.STATUS_PREFS, Context.MODE_PRIVATE)
        val restoredStatus = runCatching {
            ConnectionStatus.valueOf(prefs.getString(KEY_STATUS, ConnectionStatus.DISCONNECTED.name)!!)
        }.getOrDefault(ConnectionStatus.DISCONNECTED)
        state.value = OpenRungUiState(
            status = if (restoredStatus == ConnectionStatus.CONNECTED) ConnectionStatus.DISCONNECTED else restoredStatus,
            brokerUrl = prefs.getString(KEY_BROKER_URL, AppConfig.DEFAULT_BROKER_URL) ?: AppConfig.DEFAULT_BROKER_URL,
            // A cold start always reconnects fresh, so never restore a stale relay label (would leak a prior relay).
            relayLabel = null,
            lastError = prefs.getString(KEY_LAST_ERROR, null),
            logLines = prefs.getString(KEY_LOG_LINES, null)?.lines()?.filter { it.isNotBlank() }.orEmpty(),
            recentRegions = loadRecents(prefs.getString(KEY_RECENT_NODES, null)),
        )
    }

    fun setBrokerUrl(brokerUrl: String) {
        state.update { it.copy(brokerUrl = brokerUrl) }
        persist()
    }

    /** Updates only the relay label (e.g. resolved geo location) without emitting a status log line. */
    fun setRelayLabel(relayLabel: String?) {
        state.update { it.copy(relayLabel = relayLabel) }
        persist()
    }

    fun setStatus(
        status: ConnectionStatus,
        relayLabel: String? = state.value.relayLabel,
        lastError: String? = state.value.lastError,
    ) {
        state.update {
            it.copy(
                status = status,
                relayLabel = relayLabel,
                lastError = lastError,
            )
        }
        appendLog(status.label)
    }

    private val ConnectionStatus.label: String
        get() = appContext?.getString(labelResId) ?: name.lowercase()

    fun appendLog(message: String) {
        val timestamp = LocalTime.now().format(timeFormatter)
        state.update {
            it.copy(logLines = (it.logLines + "[$timestamp] $message").takeLast(MAX_LOG_LINES))
        }
        persist()
    }

    fun fail(message: String) {
        val context = appContext
        val logMessage = context?.getString(com.openrung.client.R.string.log_error_prefix, message) ?: "error: $message"
        state.update {
            it.copy(
                status = ConnectionStatus.FAILED,
                lastError = message,
                relayLabel = null,
                logLines = (it.logLines + "[${LocalTime.now().format(timeFormatter)}] $logMessage")
                    .takeLast(MAX_LOG_LINES),
            )
        }
        persist()
    }

    fun clearError() {
        state.update { it.copy(lastError = null) }
        persist()
    }

    /**
     * Records a location the user just connected through so it appears in the "Recents" row.
     * Deduplicates by country (most recent first) and caps the list to [AppConfig.MAX_RECENTS].
     */
    fun recordRecent(node: RecentNode) {
        state.update { current ->
            val deduped = (listOf(node) + current.recentRegions.filterNot { it.countryCode == node.countryCode })
                .take(AppConfig.MAX_RECENTS)
            current.copy(recentRegions = deduped)
        }
        persist()
    }

    /**
     * Refreshes the exit-node map directory from the broker. No-op while a load is in flight or
     * after a successful non-empty load, unless [force] is set (used by manual retry).
     */
    fun refreshDirectory(force: Boolean = false) {
        val current = state.value
        val alreadyLoaded = current.directoryStatus == DirectoryStatus.LOADED && current.availableRegions.isNotEmpty()
        if (!force && (current.directoryStatus == DirectoryStatus.LOADING || alreadyLoaded)) return

        directoryJob?.cancel()
        state.update { it.copy(directoryStatus = DirectoryStatus.LOADING) }
        directoryJob = scope.launch {
            val brokerEndpoints = AppConfig.brokerCandidates(state.value.brokerUrl)
            val directory = ExitNodeDirectory(
                fetchRelays = {
                    BrokerClient.firstReachable(brokerEndpoints, limit = AppConfig.DIRECTORY_RELAY_LIMIT).response
                },
                lookupGeo = { host -> runCatching { GeoIpClient().lookup(host) }.getOrNull() },
            )
            try {
                val regions = directory.load()
                state.update { it.copy(availableRegions = regions, directoryStatus = DirectoryStatus.LOADED) }
            } catch (cancellation: CancellationException) {
                // A superseding refresh cancelled this job; don't report it as a load failure
                // (that stale FAILED could otherwise clobber the replacement job's success).
                throw cancellation
            } catch (error: Throwable) {
                state.update { it.copy(directoryStatus = DirectoryStatus.FAILED) }
            }
        }
    }

    private fun loadRecents(serialized: String?): List<RecentNode> {
        if (serialized.isNullOrBlank()) return emptyList()
        return runCatching {
            json.decodeFromString(ListSerializer(RecentNode.serializer()), serialized)
        }.getOrDefault(emptyList())
    }

    private fun persist() {
        val context = appContext ?: return
        val current = state.value
        context.getSharedPreferences(AppConfig.STATUS_PREFS, Context.MODE_PRIVATE)
            .edit()
            .putString(KEY_STATUS, current.status.name)
            .putString(KEY_BROKER_URL, current.brokerUrl)
            .putString(KEY_RELAY_LABEL, current.relayLabel)
            .putString(KEY_LAST_ERROR, current.lastError)
            .putString(KEY_LOG_LINES, current.logLines.joinToString("\n"))
            .putString(KEY_RECENT_NODES, json.encodeToString(ListSerializer(RecentNode.serializer()), current.recentRegions))
            .apply()
    }
}
