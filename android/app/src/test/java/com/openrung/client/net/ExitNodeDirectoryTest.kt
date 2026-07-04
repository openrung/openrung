package com.openrung.client.net

import com.openrung.client.model.RelayDescriptor
import com.openrung.client.model.RelayListResponse
import com.openrung.client.sampleRelay
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ExitNodeDirectoryTest {
    private fun geo(
        countryCode: String,
        country: String = countryCode,
        latitude: Double = 0.0,
        longitude: Double = 0.0,
    ) = ClientGeoInfo(
        ip = "203.0.113.1",
        country = country,
        countryCode = countryCode,
        city = "",
        asn = "",
        isp = "",
        organization = "",
        latitude = latitude,
        longitude = longitude,
    )

    private fun response(relays: List<RelayDescriptor>) =
        RelayListResponse(count = relays.size, serverTime = "2026-01-01T00:00:00Z", relays = relays)

    @Test
    fun groupsRelaysByCountryAndPlacesAtCentroid() = runTest {
        val relays = listOf(
            sampleRelay(id = "a", publicHost = "1.1.1.1"),
            sampleRelay(id = "b", publicHost = "2.2.2.2"),
            sampleRelay(id = "c", publicHost = "3.3.3.3"),
        )
        val geoByHost = mapOf(
            "1.1.1.1" to geo("JP"),
            "2.2.2.2" to geo("JP"),
            "3.3.3.3" to geo("US"),
        )

        val regions = ExitNodeDirectory(
            fetchRelays = { response(relays) },
            lookupGeo = { host -> geoByHost[host] },
        ).load()

        assertEquals(2, regions.size)
        // Sorted by node count descending → Japan (2 nodes) first.
        val japan = regions.first()
        assertEquals("JP", japan.countryCode)
        assertEquals("Japan", japan.countryName)
        assertEquals(2, japan.nodeCount)
        assertEquals(36.20, japan.latitude, 0.001)
        assertEquals(138.25, japan.longitude, 0.001)
    }

    @Test
    fun fallsBackToGeoCoordinatesForUnknownCountry() = runTest {
        val regions = ExitNodeDirectory(
            fetchRelays = { response(listOf(sampleRelay(publicHost = "9.9.9.9"))) },
            lookupGeo = { geo("ZZ", country = "Nowhere", latitude = 12.5, longitude = 34.0) },
        ).load()

        assertEquals(1, regions.size)
        assertEquals("Nowhere", regions.first().countryName)
        assertEquals(12.5, regions.first().latitude, 0.001)
        assertEquals(34.0, regions.first().longitude, 0.001)
    }

    @Test
    fun skipsRelaysWithMissingOrBlankGeo() = runTest {
        val regions = ExitNodeDirectory(
            fetchRelays = {
                response(
                    listOf(
                        sampleRelay(id = "a", publicHost = "5.5.5.5"),
                        sampleRelay(id = "b", publicHost = "6.6.6.6"),
                    ),
                )
            },
            lookupGeo = { host -> if (host == "5.5.5.5") geo("", country = "") else null },
        ).load()

        assertTrue(regions.isEmpty())
    }
}
