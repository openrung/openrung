package com.openrung.client.net

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

data class ClientGeoInfo(
    val ip: String,
    val country: String,
    val countryCode: String,
    val city: String,
    val asn: String,
    val isp: String,
    val organization: String,
    val latitude: Double = 0.0,
    val longitude: Double = 0.0,
) {
    fun telemetryAttributes(): Map<String, String> = mapOf(
        "client_ip" to ip,
        "country" to country,
        "country_code" to countryCode,
        "city" to city,
        "asn" to asn,
        "isp" to isp,
        "organization" to organization,
    ).filterValues { it.isNotBlank() }

    /** Human-readable location such as "Austin, United States", or "" when unknown. */
    fun locationLabel(): String = listOf(city, country).filter { it.isNotBlank() }.joinToString(", ")
}

@Serializable
private data class GeoIpResponse(
    val ip: String = "",
    val success: Boolean = false,
    val country: String = "",
    @SerialName("country_code")
    val countryCode: String = "",
    val city: String = "",
    val latitude: Double = 0.0,
    val longitude: Double = 0.0,
    val connection: GeoIpConnection = GeoIpConnection(),
)

@Serializable
private data class GeoIpConnection(
    val asn: Long = 0,
    val org: String = "",
    val isp: String = "",
)

class GeoIpClient(
    private val endpoint: String = DEFAULT_ENDPOINT,
    private val json: Json = Json { ignoreUnknownKeys = true },
) {
    /**
     * Looks up geo info for [ip], or for the caller's own public IP when [ip] is null/blank.
     */
    suspend fun lookup(ip: String? = null): ClientGeoInfo = withContext(Dispatchers.IO) {
        val target = if (ip.isNullOrBlank()) endpoint else endpoint.trimEnd('/') + "/" + ip.trim()
        val connection = (URL(target).openConnection() as HttpURLConnection).apply {
            requestMethod = "GET"
            connectTimeout = 4_000
            readTimeout = 4_000
            useCaches = false
            setRequestProperty("Accept", "application/json")
        }
        try {
            val status = connection.responseCode
            if (status !in 200..299) throw IOException("geo IP HTTP $status")
            decode(connection.inputStream.bufferedReader().use { it.readText() })
        } finally {
            connection.disconnect()
        }
    }

    internal fun decode(body: String): ClientGeoInfo {
        val response = json.decodeFromString<GeoIpResponse>(body)
        if (!response.success || response.ip.isBlank()) throw IOException("geo IP lookup failed")
        return ClientGeoInfo(
            ip = response.ip,
            country = response.country,
            countryCode = response.countryCode,
            city = response.city,
            asn = response.connection.asn.takeIf { it > 0 }?.let { "AS$it" }.orEmpty(),
            isp = response.connection.isp,
            organization = response.connection.org,
            latitude = response.latitude,
            longitude = response.longitude,
        )
    }

    companion object {
        const val DEFAULT_ENDPOINT = "https://ipwho.is/"
    }
}
