package com.openrung.client.model

import java.time.Instant

class RelaySelector {
    fun orderedCandidates(relays: List<RelayDescriptor>, now: Instant): List<RelayDescriptor> =
        relays.filter { it.isUsable(now) }

    fun selectFirstUsable(relays: List<RelayDescriptor>, now: Instant): RelayDescriptor? =
        orderedCandidates(relays, now).firstOrNull()
}
