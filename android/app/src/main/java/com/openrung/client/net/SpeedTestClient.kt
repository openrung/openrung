package com.openrung.client.net

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URI
import java.net.URL

data class SpeedTestResult(
    val bytesDownloaded: Long,
    val durationMs: Long,
    val timeToFirstByteMs: Long,
    val downloadMbps: Double,
)

class SpeedTestClient(
    brokerUrl: String,
    private val warmupBytes: Int = DEFAULT_WARMUP_BYTES,
    private val measurementBytes: Int = DEFAULT_MEASUREMENT_BYTES,
) {
    private val endpoint = speedTestUrl(brokerUrl)

    suspend fun run(): SpeedTestResult = withContext(Dispatchers.IO) {
        download(warmupBytes)
        download(measurementBytes)
    }

    private fun download(bytes: Int): SpeedTestResult {
        val separator = if (endpoint.contains('?')) '&' else '?'
        val url = URL("$endpoint${separator}bytes=$bytes&cacheBust=${System.nanoTime()}")
        val connection = (url.openConnection() as HttpURLConnection).apply {
            requestMethod = "GET"
            connectTimeout = 10_000
            readTimeout = 60_000
            useCaches = false
            setRequestProperty("Accept-Encoding", "identity")
        }

        try {
            val startedNs = System.nanoTime()
            val status = connection.responseCode
            if (status !in 200..299) throw IOException("speed test HTTP $status")

            var downloaded = 0L
            var firstByteNs: Long? = null
            val buffer = ByteArray(DEFAULT_BUFFER_SIZE)
            connection.inputStream.use { input ->
                while (true) {
                    val count = input.read(buffer)
                    if (count < 0) break
                    if (count == 0) continue
                    if (firstByteNs == null) firstByteNs = System.nanoTime()
                    downloaded += count
                }
            }
            val finishedNs = System.nanoTime()
            if (downloaded <= 0) throw IOException("speed test returned no data")
            val durationNs = (finishedNs - startedNs).coerceAtLeast(1L)
            return SpeedTestResult(
                bytesDownloaded = downloaded,
                durationMs = durationNs / 1_000_000,
                timeToFirstByteMs = ((firstByteNs ?: finishedNs) - startedNs) / 1_000_000,
                downloadMbps = calculateMbps(downloaded, durationNs),
            )
        } finally {
            connection.disconnect()
        }
    }

    companion object {
        const val DEFAULT_WARMUP_BYTES = 1_000_000
        const val DEFAULT_MEASUREMENT_BYTES = 10_000_000

        fun speedTestUrl(baseUrl: String): String {
            val uri = URI(baseUrl.trim())
            require(!uri.scheme.isNullOrBlank() && !uri.host.isNullOrBlank()) {
                "broker URL must include scheme and host"
            }
            val basePath = uri.rawPath.orEmpty().trim('/')
            val path = listOf(basePath, "api/v1/speed-test")
                .filter { it.isNotBlank() }
                .joinToString(separator = "/", prefix = "/")
            return URI(uri.scheme, uri.userInfo, uri.host, uri.port, path, null, null).toString()
        }

        fun calculateMbps(bytes: Long, durationNs: Long): Double {
            require(bytes >= 0) { "bytes must not be negative" }
            require(durationNs > 0) { "duration must be positive" }
            return bytes.toDouble() * 8.0 * 1_000.0 / durationNs.toDouble()
        }
    }
}
