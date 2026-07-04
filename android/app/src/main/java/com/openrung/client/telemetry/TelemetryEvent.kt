package com.openrung.client.telemetry

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
data class TelemetryEvent(
    @SerialName("schema_version")
    val schemaVersion: Int = 1,
    @SerialName("event_id")
    val eventId: String,
    val event: String,
    @SerialName("occurred_at")
    val occurredAt: String,
    @SerialName("client_id")
    val clientId: String,
    @SerialName("session_id")
    val sessionId: String,
    @SerialName("relay_id")
    val relayId: String? = null,
    @SerialName("application_package")
    val applicationPackage: String? = null,
    @SerialName("application_uid")
    val applicationUid: Int? = null,
    @SerialName("destination_ip")
    val destinationIp: String? = null,
    @SerialName("destination_port")
    val destinationPort: Int? = null,
    val protocol: String? = null,
    val attributes: Map<String, String> = emptyMap(),
    val measurements: Map<String, Long> = emptyMap(),
)

@Serializable
data class TelemetryBatch(val events: List<TelemetryEvent>)
