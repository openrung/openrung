package com.openrung.client.net

import com.openrung.client.model.RelayConstants
import com.openrung.client.model.RelayDescriptor
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

data class SingBoxConfiguration(
    val relay: RelayDescriptor,
    val tunnelIPv4Address: String = "172.19.0.1/30",
    val tunnelIPv6Address: String = "fdfe:dcba:9876::1/126",
    val dnsServers: List<String> = listOf("1.1.1.1", "8.8.8.8"),
    val mtu: Int = 1500,
) {
    fun encodedJsonString(): String {
        validateRelay()
        return prettyJson.encodeToString(makeJsonObject())
    }

    fun makeJsonObject(): JsonObject {
        require(mtu > 0) { "mtu must be positive" }
        validateRelay()

        val tunInbound = mutableMapOf<String, JsonElement>(
            "type" to JsonPrimitive("tun"),
            "tag" to JsonPrimitive("tun-in"),
            "address" to JsonArray(listOf(JsonPrimitive(tunnelIPv4Address), JsonPrimitive(tunnelIPv6Address))),
            "mtu" to JsonPrimitive(mtu),
            "auto_route" to JsonPrimitive(true),
            "strict_route" to JsonPrimitive(true),
            "stack" to JsonPrimitive("system"),
            "dns_mode" to JsonPrimitive("hijack"),
            "endpoint_independent_nat" to JsonPrimitive(true),
        )
        relayRouteExcludeAddress(relay.publicHost)?.let {
            tunInbound["route_exclude_address"] = JsonArray(listOf(JsonPrimitive(it)))
        }

        return buildJsonObject {
            put("log", buildJsonObject {
                put("level", "info")
                put("timestamp", true)
            })
            put("dns", buildJsonObject {
                put("servers", buildJsonArray {
                    dnsServers.forEachIndexed { index, server ->
                        add(buildJsonObject {
                            put("tag", "dns-$index")
                            put("type", "tcp")
                            put("server", server)
                            put("detour", "proxy")
                        })
                    }
                })
                put("final", "dns-0")
            })
            put("inbounds", JsonArray(listOf(JsonObject(tunInbound))))
            put("outbounds", buildJsonArray {
                add(buildJsonObject {
                    put("type", "vless")
                    put("tag", "proxy")
                    put("server", relay.publicHost)
                    put("server_port", relay.publicPort)
                    put("uuid", relay.clientId)
                    put("flow", relay.flow)
                    put("network", "tcp")
                    put("packet_encoding", "xudp")
                    put("tls", buildJsonObject {
                        put("enabled", true)
                        put("server_name", relay.serverName)
                        put("utls", buildJsonObject {
                            put("enabled", true)
                            put("fingerprint", "chrome")
                        })
                        put("reality", buildJsonObject {
                            put("enabled", true)
                            put("public_key", relay.realityPublicKey)
                            put("short_id", relay.shortId)
                        })
                    })
                })
                add(buildJsonObject {
                    put("type", "direct")
                    put("tag", "direct")
                })
                add(buildJsonObject {
                    put("type", "block")
                    put("tag", "block")
                })
            })
            put("route", buildJsonObject {
                put("auto_detect_interface", true)
                put("find_process", true)
                put("default_domain_resolver", "dns-0")
                put("rules", buildJsonArray {
                    add(buildJsonObject {
                        put("protocol", "dns")
                        put("action", "hijack-dns")
                    })
                })
                put("final", "proxy")
            })
        }
    }

    private fun validateRelay() {
        require(relay.relayProtocol == RelayConstants.PROTOCOL_VLESS_REALITY_VISION) {
            "relay protocol is not vless-reality-vision"
        }
        require(relay.flow == RelayConstants.FLOW_VISION) {
            "relay flow is not xtls-rprx-vision"
        }
        require(relay.exitMode == RelayConstants.EXIT_MODE_DIRECT) {
            "relay exit mode is not direct"
        }
        require(relay.publicHost.isNotBlank() && relay.publicPort > 0) {
            "relay is missing required connection fields"
        }
        require(relay.clientId.isNotBlank() && relay.realityPublicKey.isNotBlank()) {
            "relay is missing required Reality fields"
        }
        require(relay.shortId.isNotBlank() && relay.serverName.isNotBlank()) {
            "relay is missing required TLS fields"
        }
    }

    companion object {
        private val prettyJson = Json {
            prettyPrint = true
        }

        fun relayRouteExcludeAddress(host: String): String? {
            val cleanHost = host.removePrefix("[").removeSuffix("]")
            return when {
                cleanHost.isIPv4Literal() -> "$cleanHost/32"
                cleanHost.contains(":") -> "$cleanHost/128"
                else -> null
            }
        }

        private fun String.isIPv4Literal(): Boolean {
            val octets = split(".")
            return octets.size == 4 && octets.all { octet ->
                val value = octet.toIntOrNull()
                value != null && value in 0..255 && value.toString() == octet
            }
        }
    }
}
