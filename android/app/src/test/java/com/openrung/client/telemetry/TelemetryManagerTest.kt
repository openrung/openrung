package com.openrung.client.telemetry

import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class TelemetryManagerTest {
    @Test
    fun buildsConnectedSessionHeartbeatWithMetadataAndDurations() {
        val session = TelemetryManager.Session(
            id = "session-1",
            clientId = "client-1",
            brokerUrl = "https://broker.example.com",
            startedElapsedMs = 1_000,
            relayId = "relay-1",
            connectedElapsedMs = 2_000,
        )

        val event = buildSessionHeartbeat(
            session = session,
            occurredAt = Instant.parse("2026-06-22T12:00:00Z"),
            elapsedRealtimeMs = 62_000,
            attributes = mapOf("android_api" to "37", "city" to "Austin"),
        )!!

        assertEquals("session_heartbeat", event.event)
        assertEquals("client-1", event.clientId)
        assertEquals("session-1", event.sessionId)
        assertEquals("relay-1", event.relayId)
        assertEquals("connected", event.attributes["connection_state"])
        assertEquals("37", event.attributes["android_api"])
        assertEquals("Austin", event.attributes["city"])
        assertEquals(61_000L, event.measurements["session_duration_ms"])
        assertEquals(60_000L, event.measurements["connected_duration_ms"])
    }

    @Test
    fun doesNotBuildHeartbeatBeforeConnection() {
        val session = TelemetryManager.Session(
            id = "session-1",
            clientId = "client-1",
            brokerUrl = "https://broker.example.com",
            startedElapsedMs = 1_000,
        )

        assertNull(buildSessionHeartbeat(session, Instant.EPOCH, 2_000, emptyMap()))
    }
}
