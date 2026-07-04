package com.openrung.client

import com.openrung.client.model.RelayConstants
import com.openrung.client.model.RelayDescriptor

fun sampleRelay(
    id: String = "relay-1",
    publicHost: String = "203.0.113.10",
    expiresAt: String = "2030-01-01T00:00:00Z",
    relayProtocol: String = RelayConstants.PROTOCOL_VLESS_REALITY_VISION,
    flow: String = RelayConstants.FLOW_VISION,
    exitMode: String = RelayConstants.EXIT_MODE_DIRECT,
): RelayDescriptor = RelayDescriptor(
    id = id,
    publicHost = publicHost,
    publicPort = 443,
    relayProtocol = relayProtocol,
    clientId = "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
    realityPublicKey = "dev-public-key",
    shortId = "5f7a8d9c01ab23cd",
    serverName = "www.cloudflare.com",
    flow = flow,
    exitMode = exitMode,
    maxSessions = 10,
    maxMbps = 100,
    volunteerVersion = "dev",
    registeredAt = "2026-01-01T00:00:00Z",
    lastHeartbeatAt = "2026-01-01T00:00:00Z",
    expiresAt = expiresAt,
)
