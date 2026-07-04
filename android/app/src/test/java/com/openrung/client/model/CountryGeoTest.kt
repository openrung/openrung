package com.openrung.client.model

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Test

class CountryGeoTest {
    @Test
    fun resolvesKnownCountryCaseInsensitively() {
        val jp = CountryGeo.centroid("jp")
        assertNotNull(jp)
        assertEquals("Japan", jp!!.name)
        assertEquals(36.20, jp.latitude, 0.001)
        assertEquals("Japan", CountryGeo.displayName("JP"))
    }

    @Test
    fun trimsWhitespaceBeforeLookup() {
        assertEquals("Singapore", CountryGeo.displayName("  sg "))
    }

    @Test
    fun returnsNullForUnknownCountry() {
        assertNull(CountryGeo.centroid("ZZ"))
        assertNull(CountryGeo.displayName("ZZ"))
    }
}
