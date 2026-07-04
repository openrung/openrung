package com.openrung.client.net

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.SystemClock
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

data class InternetProbeResult(
    val endpoint: String,
    val durationMs: Long,
)

class InternetProbe(context: Context) {
    private val connectivityManager = context.getSystemService(ConnectivityManager::class.java)

    suspend fun verify(): InternetProbeResult {
        val started = SystemClock.elapsedRealtime()
        val deadline = started + PROBE_DEADLINE_MS
        var lastError: Throwable? = null

        while (SystemClock.elapsedRealtime() < deadline) {
            val vpnNetwork = currentVpnNetwork()
            if (vpnNetwork == null) {
                delay(RETRY_DELAY_MS)
                continue
            }

            for (endpoint in ENDPOINTS) {
                try {
                    probe(vpnNetwork, endpoint)
                    return InternetProbeResult(
                        endpoint = endpoint,
                        durationMs = SystemClock.elapsedRealtime() - started,
                    )
                } catch (error: CancellationException) {
                    throw error
                } catch (error: Throwable) {
                    lastError = error
                }
            }
            delay(RETRY_DELAY_MS)
        }

        throw IOException(
            "VPN started, but the internet probe failed" +
                (lastError?.message?.let { ": $it" } ?: ""),
            lastError,
        )
    }

    private fun currentVpnNetwork(): Network? =
        connectivityManager.allNetworks.firstOrNull { network ->
            connectivityManager.getNetworkCapabilities(network)
                ?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true
        }

    private suspend fun probe(network: Network, endpoint: String) = withContext(Dispatchers.IO) {
        val connection = (network.openConnection(URL(endpoint)) as HttpURLConnection).apply {
            requestMethod = "GET"
            connectTimeout = REQUEST_TIMEOUT_MS
            readTimeout = REQUEST_TIMEOUT_MS
            instanceFollowRedirects = false
            useCaches = false
            setRequestProperty("Cache-Control", "no-cache")
        }

        try {
            val status = connection.responseCode
            if (!acceptsHttpStatus(status)) {
                throw IOException("internet probe HTTP $status from $endpoint")
            }
            connection.inputStream.use { input -> input.read() }
        } finally {
            connection.disconnect()
        }
    }

    companion object {
        private const val PROBE_DEADLINE_MS = 12_000L
        private const val RETRY_DELAY_MS = 500L
        private const val REQUEST_TIMEOUT_MS = 3_000

        internal val ENDPOINTS = listOf(
            "https://www.gstatic.com/generate_204",
            "https://cp.cloudflare.com/generate_204",
        )

        internal fun acceptsHttpStatus(status: Int): Boolean = status in 200..299
    }
}
