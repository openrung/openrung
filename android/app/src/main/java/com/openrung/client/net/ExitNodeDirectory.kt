package com.openrung.client.net

import com.openrung.client.model.CountryGeo
import com.openrung.client.model.ExitNodeRegion
import com.openrung.client.model.RelayListResponse
import com.openrung.client.model.RelaySelector
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope

/**
 * Builds the exit-node map directory: fetches the broker's relay list, resolves each usable relay's
 * country via GeoIP, and groups them into one [ExitNodeRegion] per country (placed at a curated
 * centroid, falling back to the GeoIP coordinate when the country is not in [CountryGeo]).
 *
 * Both the relay fetch and the geo lookup are injected so this stays free of network/Android
 * dependencies and is unit-testable.
 */
class ExitNodeDirectory(
    private val fetchRelays: suspend () -> RelayListResponse,
    private val lookupGeo: suspend (host: String) -> ClientGeoInfo?,
    private val selector: RelaySelector = RelaySelector(),
) {
    suspend fun load(): List<ExitNodeRegion> = coroutineScope {
        val response = fetchRelays()
        val usable = selector.orderedCandidates(response.relays, response.serverInstant)
        if (usable.isEmpty()) return@coroutineScope emptyList()

        // Resolve each distinct host once, concurrently.
        val geoByHost = usable.map { it.publicHost }.distinct()
            .map { host -> async { host to lookupGeo(host) } }
            .awaitAll()
            .toMap()

        usable
            .mapNotNull { relay ->
                val geo = geoByHost[relay.publicHost] ?: return@mapNotNull null
                val code = geo.countryCode.trim().uppercase().ifBlank { return@mapNotNull null }
                code to geo
            }
            .groupBy({ it.first }, { it.second })
            .map { (code, geos) ->
                val centroid = CountryGeo.centroid(code)
                val first = geos.first()
                ExitNodeRegion(
                    countryCode = code,
                    countryName = centroid?.name ?: first.country.ifBlank { code },
                    latitude = centroid?.latitude ?: first.latitude,
                    longitude = centroid?.longitude ?: first.longitude,
                    nodeCount = geos.size,
                )
            }
            .sortedWith(compareByDescending<ExitNodeRegion> { it.nodeCount }.thenBy { it.countryName })
    }
}
