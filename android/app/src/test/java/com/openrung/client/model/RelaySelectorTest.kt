package com.openrung.client.model

import com.openrung.client.sampleRelay
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant

class RelaySelectorTest {
    private val now = Instant.parse("2026-01-01T00:00:00Z")

    @Test
    fun filtersUnusableRelays() {
        val usable = sampleRelay(id = "usable")
        val expired = sampleRelay(id = "expired", expiresAt = "2025-01-01T00:00:00Z")
        val wrongProtocol = sampleRelay(id = "wrong", relayProtocol = "socks")

        val candidates = RelaySelector().orderedCandidates(
            relays = listOf(expired, wrongProtocol, usable),
            now = now,
        )

        assertEquals(listOf("usable"), candidates.map { it.id })
    }

    @Test
    fun preservesBrokerRankedOrder() {
        val ipv4 = sampleRelay(id = "ipv4", publicHost = "203.0.113.10")
        val dns = sampleRelay(id = "dns", publicHost = "relay.example.com")
        val ipv6 = sampleRelay(id = "ipv6", publicHost = "[2001:db8::1]")

        val candidates = RelaySelector().orderedCandidates(
            relays = listOf(ipv4, dns, ipv6),
            now = now,
        )

        assertEquals(listOf("ipv4", "dns", "ipv6"), candidates.map { it.id })
    }

    @Test
    fun relayUsabilityRequiresConnectionFields() {
        assertTrue(sampleRelay().isUsable(now))
        assertFalse(sampleRelay(publicHost = "").isUsable(now))
        assertFalse(sampleRelay(exitMode = "dedicated").isUsable(now))
    }
}
