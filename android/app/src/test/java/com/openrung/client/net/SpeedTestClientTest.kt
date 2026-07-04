package com.openrung.client.net

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class SpeedTestClientTest {
    @Test
    fun buildsSpeedTestUrlFromBrokerBasePath() {
        assertEquals(
            "https://broker.example.com/openrung/api/v1/speed-test",
            SpeedTestClient.speedTestUrl("https://broker.example.com/openrung/"),
        )
    }

    @Test
    fun calculatesMegabitsPerSecond() {
        assertEquals(
            8.0,
            SpeedTestClient.calculateMbps(bytes = 1_000_000, durationNs = 1_000_000_000),
            0.0001,
        )
    }

    @Test
    fun rejectsZeroDuration() {
        assertThrows(IllegalArgumentException::class.java) {
            SpeedTestClient.calculateMbps(bytes = 1_000_000, durationNs = 0)
        }
    }
}
