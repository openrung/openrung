package com.openrung.client.telemetry

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URI
import java.net.URL

class TelemetryClient(
    private val baseUrl: String,
    private val json: Json = Json { encodeDefaults = true },
) {
    suspend fun send(events: List<TelemetryEvent>) = withContext(Dispatchers.IO) {
        if (events.isEmpty()) return@withContext
        val connection = (URL(telemetryUrl(baseUrl)).openConnection() as HttpURLConnection).apply {
            requestMethod = "POST"
            connectTimeout = 10_000
            readTimeout = 15_000
            doOutput = true
            setRequestProperty("Content-Type", "application/json")
            setRequestProperty("X-OpenRung-Client-ID", events.first().clientId)
            setRequestProperty("X-OpenRung-Session-ID", events.first().sessionId)
        }
        try {
            connection.outputStream.bufferedWriter().use {
                it.write(json.encodeToString(TelemetryBatch(events)))
            }
            val status = connection.responseCode
            if (status !in 200..299) {
                val body = connection.errorStream?.bufferedReader()?.use { it.readText() }.orEmpty()
                throw IOException("broker telemetry: ${body.ifBlank { connection.responseMessage }}")
            }
            connection.inputStream.close()
        } finally {
            connection.disconnect()
        }
    }

    companion object {
        fun telemetryUrl(baseUrl: String): String {
            val uri = URI(baseUrl.trim())
            require(!uri.scheme.isNullOrBlank() && !uri.host.isNullOrBlank()) {
                "broker URL must include scheme and host"
            }
            val basePath = uri.rawPath.orEmpty().trim('/')
            val path = listOf(basePath, "api/v1/telemetry/events")
                .filter { it.isNotBlank() }
                .joinToString(separator = "/", prefix = "/")
            return URI(uri.scheme, uri.userInfo, uri.host, uri.port, path, null, null).toString()
        }
    }
}
