package com.openrung.client.net

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class BrokerClientTest {
    @Test
    fun buildsRelayListUrlForPlainBroker() {
        assertEquals(
            "http://localhost:8080/api/v1/relays?limit=5",
            BrokerClient.relayListUrl("http://localhost:8080", 5),
        )
    }

    @Test
    fun preservesBrokerBasePath() {
        assertEquals(
            "https://example.com/openrung/api/v1/relays?limit=10",
            BrokerClient.relayListUrl("https://example.com/openrung/", 10),
        )
    }

    @Test
    fun defaultsInvalidLimitToFive() {
        assertEquals(
            "https://example.com/api/v1/relays?limit=5",
            BrokerClient.relayListUrl("https://example.com", 0),
        )
    }

    @Test
    fun replacesExistingLimitQuery() {
        assertEquals(
            "https://example.com/api/v1/relays?foo=bar&limit=8",
            BrokerClient.relayListUrl("https://example.com?foo=bar&limit=1", 8),
        )
    }

    @Test
    fun rejectsBrokerWithoutHost() {
        assertThrows(IllegalArgumentException::class.java) {
            BrokerClient.relayListUrl("localhost:8080", 5)
        }
    }

    @Test
    fun candidatesPutPrimaryFirstThenFallbacks() {
        assertEquals(
            listOf("https://primary.example/", "http://fallback-a/", "http://fallback-b/"),
            BrokerClient.candidates(
                "https://primary.example/",
                listOf("http://fallback-a/", "http://fallback-b/"),
            ),
        )
    }

    @Test
    fun candidatesDeduplicatePrimaryThatIsAlsoAFallback() {
        assertEquals(
            listOf("http://fallback-a/", "http://fallback-b/"),
            BrokerClient.candidates(
                "http://fallback-a/",
                listOf("http://fallback-a/", "http://fallback-b/"),
            ),
        )
    }

    @Test
    fun candidatesKeepDefaultOrderWhenPrimaryEchoesANonFirstDefault() {
        // Migration guard: an upgrader whose persisted "primary" is the old raw-IP default must still
        // get the HTTPS-fronted endpoint first, not the IP.
        assertEquals(
            listOf("https://broker.example/", "http://203.0.113.10:8080/"),
            BrokerClient.candidates(
                "http://203.0.113.10:8080/",
                listOf("https://broker.example/", "http://203.0.113.10:8080/"),
            ),
        )
    }

    @Test
    fun candidatesIgnoreBlankPrimary() {
        assertEquals(
            listOf("http://fallback-a/"),
            BrokerClient.candidates("   ", listOf("http://fallback-a/")),
        )
        assertEquals(
            listOf("http://fallback-a/"),
            BrokerClient.candidates(null, listOf("http://fallback-a/")),
        )
    }
}
