package com.openrung.client.model

import kotlinx.serialization.Serializable

/**
 * One country/region on the exit-node map. Volunteer relays are grouped by the country resolved for
 * their public host, so a single marker may stand for several nodes ([nodeCount]).
 */
data class ExitNodeRegion(
    val countryCode: String,
    val countryName: String,
    val latitude: Double,
    val longitude: Double,
    val nodeCount: Int,
)

/** A location the user has previously connected through, shown in the main-screen "Recents" row. */
@Serializable
data class RecentNode(
    val countryCode: String,
    val label: String,
    val latitude: Double,
    val longitude: Double,
)
