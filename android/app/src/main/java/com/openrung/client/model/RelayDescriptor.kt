package com.openrung.client.model

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import java.time.Instant

object RelayConstants {
    const val PROTOCOL_VLESS_REALITY_VISION = "vless-reality-vision"
    const val FLOW_VISION = "xtls-rprx-vision"
    const val EXIT_MODE_DIRECT = "direct"
}

@Serializable
data class RelayDescriptor(
    val id: String,
    @SerialName("public_host")
    val publicHost: String,
    @SerialName("public_port")
    val publicPort: Int,
    @SerialName("protocol")
    val relayProtocol: String,
    @SerialName("client_id")
    val clientId: String,
    @SerialName("reality_public_key")
    val realityPublicKey: String,
    @SerialName("short_id")
    val shortId: String,
    @SerialName("server_name")
    val serverName: String,
    val flow: String,
    @SerialName("exit_mode")
    val exitMode: String,
    @SerialName("max_sessions")
    val maxSessions: Int,
    @SerialName("max_mbps")
    val maxMbps: Int,
    @SerialName("volunteer_version")
    val volunteerVersion: String,
    @SerialName("registered_at")
    val registeredAt: String,
    @SerialName("last_heartbeat_at")
    val lastHeartbeatAt: String,
    @SerialName("expires_at")
    val expiresAt: String,
) {
    fun isUsable(now: Instant): Boolean {
        val expires = runCatching { Instant.parse(expiresAt) }.getOrNull() ?: return false
        return relayProtocol == RelayConstants.PROTOCOL_VLESS_REALITY_VISION &&
            flow == RelayConstants.FLOW_VISION &&
            exitMode == RelayConstants.EXIT_MODE_DIRECT &&
            expires > now &&
            publicHost.isNotBlank() &&
            publicPort > 0 &&
            clientId.isNotBlank() &&
            realityPublicKey.isNotBlank() &&
            shortId.isNotBlank() &&
            serverName.isNotBlank()
    }
}

@Serializable
data class RelayListResponse(
    val count: Int,
    @SerialName("server_time")
    val serverTime: String,
    val relays: List<RelayDescriptor>,
) {
    val serverInstant: Instant
        get() = Instant.parse(serverTime)
}

@Serializable
data class ErrorResponse(
    val error: String = "",
)
