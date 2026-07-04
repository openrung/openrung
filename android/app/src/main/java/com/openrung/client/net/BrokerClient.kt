package com.openrung.client.net

import android.os.Build
import com.openrung.client.BuildConfig
import com.openrung.client.model.ErrorResponse
import com.openrung.client.model.RelayListResponse
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URI
import java.net.URL
import java.net.URLEncoder

class BrokerClient(
    private val baseUrl: String,
    private val json: Json = Json { ignoreUnknownKeys = true },
) {
    suspend fun listRelays(
        limit: Int = 5,
        clientId: String? = null,
        sessionId: String? = null,
    ): RelayListResponse = withContext(Dispatchers.IO) {
        val url = URL(relayListUrl(baseUrl, limit))
        val connection = (url.openConnection() as HttpURLConnection).apply {
            requestMethod = "GET"
            connectTimeout = 10_000
            readTimeout = 15_000
            clientId?.let { setRequestProperty("X-OpenRung-Client-ID", it) }
            sessionId?.let { setRequestProperty("X-OpenRung-Session-ID", it) }
            setRequestProperty("X-OpenRung-App-Version", BuildConfig.VERSION_NAME)
            setRequestProperty("X-OpenRung-Android-API", Build.VERSION.SDK_INT.toString())
        }

        try {
            val status = connection.responseCode
            val stream = if (status in 200..299) {
                connection.inputStream
            } else {
                connection.errorStream ?: connection.inputStream
            }
            val body = stream.bufferedReader().use { it.readText() }
            if (status !in 200..299) {
                val apiError = runCatching { json.decodeFromString<ErrorResponse>(body).error }.getOrNull()
                throw IOException("broker list relays: ${apiError?.ifBlank { null } ?: body.ifBlank { connection.responseMessage }}")
            }
            json.decodeFromString<RelayListResponse>(body)
        } finally {
            connection.disconnect()
        }
    }

    /** A successful relay fetch together with the broker endpoint that served it. */
    data class Fetch(val brokerUrl: String, val response: RelayListResponse)

    companion object {
        /**
         * Builds the ordered broker candidate list, de-duplicated while preserving order. A non-blank
         * [primary] is tried FIRST only when it is a genuine override — i.e. not already one of the
         * [fallbacks]. A persisted value that merely echoes a built-in default must NOT reorder the
         * defaults' preferred (HTTPS-first) ordering, otherwise an upgrader whose last-used default was
         * the raw IP would keep hitting the IP before the Cloudflare-fronted endpoint. Pure and
         * side-effect free so it is unit-testable.
         */
        fun candidates(primary: String?, fallbacks: List<String>): List<String> {
            val ordered = LinkedHashSet<String>()
            val trimmedPrimary = primary?.trim()?.takeIf { it.isNotEmpty() }
            if (trimmedPrimary != null && fallbacks.none { it.trim() == trimmedPrimary }) {
                ordered.add(trimmedPrimary)
            }
            fallbacks.forEach { fallback ->
                fallback.trim().takeIf { it.isNotEmpty() }?.let { ordered.add(it) }
            }
            return ordered.toList()
        }

        /**
         * Fetches relays from each candidate broker in order, returning the first success along with
         * the endpoint that served it. A blocked or down primary endpoint therefore no longer takes
         * discovery offline as long as one candidate is reachable. Rethrows cancellation immediately;
         * if every candidate fails, the last error is rethrown.
         */
        suspend fun firstReachable(
            candidates: List<String>,
            limit: Int = 5,
            clientId: String? = null,
            sessionId: String? = null,
            json: Json = Json { ignoreUnknownKeys = true },
        ): Fetch {
            require(candidates.isNotEmpty()) { "no broker endpoints configured" }
            var lastError: Throwable? = null
            for (url in candidates) {
                try {
                    val response = BrokerClient(url, json).listRelays(limit, clientId, sessionId)
                    return Fetch(url, response)
                } catch (cancellation: CancellationException) {
                    throw cancellation
                } catch (error: Throwable) {
                    lastError = error
                }
            }
            throw lastError ?: IOException("no broker endpoints reachable")
        }

        fun relayListUrl(baseUrl: String, limit: Int): String {
            val trimmed = baseUrl.trim()
            require(trimmed.isNotBlank()) { "broker URL is required" }

            val uri = URI(trimmed)
            require(!uri.scheme.isNullOrBlank() && !uri.host.isNullOrBlank()) {
                "broker URL must include scheme and host"
            }

            val basePath = uri.rawPath.orEmpty().trim('/')
            val relayPath = listOf(basePath, "api/v1/relays")
                .filter { it.isNotBlank() }
                .joinToString(separator = "/", prefix = "/")
            val safeLimit = if (limit < 1) 5 else limit
            val query = appendLimit(uri.rawQuery, safeLimit)
            return URI(uri.scheme, uri.userInfo, uri.host, uri.port, relayPath, query, null).toString()
        }

        private fun appendLimit(rawQuery: String?, limit: Int): String {
            val encodedLimit = URLEncoder.encode(limit.toString(), Charsets.UTF_8.name())
            val existing = rawQuery
                ?.split("&")
                ?.filter { it.isNotBlank() }
                ?.filterNot { it.substringBefore("=") == "limit" }
                .orEmpty()
            return (existing + "limit=$encodedLimit")
                .joinToString("&")
        }
    }
}
