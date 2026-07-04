package com.openrung.client.telemetry

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class TelemetryClientTest {
    @Test
    fun buildsTelemetryUrlForPlainBroker() {
        assertEquals(
            "http://localhost:8080/api/v1/telemetry/events",
            TelemetryClient.telemetryUrl("http://localhost:8080"),
        )
    }

    @Test
    fun preservesBrokerBasePath() {
        assertEquals(
            "https://example.com/openrung/api/v1/telemetry/events",
            TelemetryClient.telemetryUrl("https://example.com/openrung/"),
        )
    }

    @Test
    fun rejectsBrokerWithoutHost() {
        assertThrows(IllegalArgumentException::class.java) {
            TelemetryClient.telemetryUrl("localhost:8080")
        }
    }
}
