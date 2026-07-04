package com.openrung.client.net

import com.openrung.client.sampleRelay
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Test

class SingBoxConfigurationTest {
    @Test
    fun buildsVlessRealityVisionConfig() {
        val relay = sampleRelay(publicHost = "203.0.113.10")
        val config = Json.parseToJsonElement(SingBoxConfiguration(relay).encodedJsonString()).jsonObject

        val inbound = config["inbounds"]!!.jsonArray[0].jsonObject
        assertEquals("tun", inbound["type"]!!.jsonPrimitive.content)
        assertEquals("hijack", inbound["dns_mode"]!!.jsonPrimitive.content)
        assertEquals(true, inbound["strict_route"]!!.jsonPrimitive.boolean)
        assertEquals("203.0.113.10/32", inbound["route_exclude_address"]!!.jsonArray[0].jsonPrimitive.content)

        val dns = config["dns"]!!.jsonObject
        val firstDnsServer = dns["servers"]!!.jsonArray[0].jsonObject
        assertEquals("tcp", firstDnsServer["type"]!!.jsonPrimitive.content)
        assertEquals("proxy", firstDnsServer["detour"]!!.jsonPrimitive.content)

        val proxy = config["outbounds"]!!.jsonArray[0].jsonObject
        assertEquals("vless", proxy["type"]!!.jsonPrimitive.content)
        assertEquals(443, proxy["server_port"]!!.jsonPrimitive.int)
        assertEquals("xudp", proxy["packet_encoding"]!!.jsonPrimitive.content)

        val tls = proxy["tls"]!!.jsonObject
        assertEquals(true, tls["enabled"]!!.jsonPrimitive.boolean)
        assertEquals("chrome", tls["utls"]!!.jsonObject["fingerprint"]!!.jsonPrimitive.content)
        assertEquals("dev-public-key", tls["reality"]!!.jsonObject["public_key"]!!.jsonPrimitive.content)

        val route = config["route"]!!.jsonObject
        assertEquals(true, route["find_process"]!!.jsonPrimitive.boolean)
        assertEquals("dns-0", route["default_domain_resolver"]!!.jsonPrimitive.content)
        assertEquals("hijack-dns", route["rules"]!!.jsonArray[0].jsonObject["action"]!!.jsonPrimitive.content)
        assertEquals("proxy", route["final"]!!.jsonPrimitive.content)
    }

    @Test
    fun excludesIpv6LiteralRelayWith128Prefix() {
        assertEquals(
            "2001:db8::1/128",
            SingBoxConfiguration.relayRouteExcludeAddress("[2001:db8::1]"),
        )
    }

    @Test
    fun doesNotExcludeDnsNameRelay() {
        assertNull(SingBoxConfiguration.relayRouteExcludeAddress("relay.example.com"))
    }

    @Test
    fun rejectsWrongRelayProtocol() {
        assertThrows(IllegalArgumentException::class.java) {
            SingBoxConfiguration(sampleRelay(relayProtocol = "socks")).encodedJsonString()
        }
    }

    @Test
    fun rejectsNonPositiveMtu() {
        assertThrows(IllegalArgumentException::class.java) {
            SingBoxConfiguration(sampleRelay(), mtu = 0).encodedJsonString()
        }
    }
}
