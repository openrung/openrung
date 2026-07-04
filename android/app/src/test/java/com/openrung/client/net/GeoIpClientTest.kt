package com.openrung.client.net

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test
import java.io.IOException

class GeoIpClientTest {
    @Test
    fun decodesClientGeographyAndNetworkOwner() {
        val result = GeoIpClient().decode(
            """
            {
              "ip": "136.41.98.75",
              "success": true,
              "country": "United States",
              "country_code": "US",
              "city": "Austin",
              "connection": {
                "asn": 16591,
                "org": "Google Fiber Inc",
                "isp": "Google Fiber Inc."
              }
            }
            """.trimIndent(),
        )

        assertEquals("136.41.98.75", result.ip)
        assertEquals("United States", result.country)
        assertEquals("Austin", result.city)
        assertEquals("AS16591", result.asn)
        assertEquals("Google Fiber Inc.", result.isp)
    }

    @Test
    fun rejectsFailedLookup() {
        assertThrows(IOException::class.java) {
            GeoIpClient().decode("""{"success":false,"ip":""}""")
        }
    }

    @Test
    fun locationLabelJoinsCityAndCountry() {
        val info = GeoIpClient().decode(
            """{"ip":"1.1.1.1","success":true,"country":"Japan","country_code":"JP","city":"Tokyo","connection":{}}""",
        )

        assertEquals("Tokyo, Japan", info.locationLabel())
    }

    @Test
    fun locationLabelOmitsBlankCity() {
        val info = GeoIpClient().decode(
            """{"ip":"1.1.1.1","success":true,"country":"Japan","country_code":"JP","city":"","connection":{}}""",
        )

        assertEquals("Japan", info.locationLabel())
    }
}
