package com.openrung.client.net

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class InternetProbeTest {
    @Test
    fun acceptsSuccessfulHttpStatusesOnly() {
        assertTrue(InternetProbe.acceptsHttpStatus(200))
        assertTrue(InternetProbe.acceptsHttpStatus(204))
        assertFalse(InternetProbe.acceptsHttpStatus(302))
        assertFalse(InternetProbe.acceptsHttpStatus(500))
    }

    @Test
    fun hasIndependentProbeEndpoints() {
        assertTrue(InternetProbe.ENDPOINTS.any { it.contains("gstatic.com") })
        assertTrue(InternetProbe.ENDPOINTS.any { it.contains("cloudflare.com") })
    }
}
