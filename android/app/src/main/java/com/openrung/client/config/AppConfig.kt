package com.openrung.client.config

object AppConfig {
    /**
     * Discovery broker (relay-list bootstrap) default. Prefer the HTTPS, Cloudflare-fronted endpoint:
     * discovery is the censorship-critical path — it runs BEFORE the VPN tunnel is up — and TLS + CDN
     * edge IPs make it costly to block. Falls back to the raw origin IP; see [DEFAULT_BROKER_URLS].
     */
    const val DEFAULT_BROKER_URL = "https://broker.openrung.org/"

    /**
     * Telemetry / heartbeat / speed-test target: the raw origin IP, NOT the Cloudflare-fronted
     * hostname. Heartbeats fire ~once/minute per connected user; routing them through the Cloudflare
     * Worker would burn the Workers free-tier quota (100k requests/day). They ride the established VPN
     * tunnel so they don't need the CDN front (and it's the same broker either way). Discovery stays
     * fronted (low volume, pre-tunnel, needs the resilience); telemetry goes direct (high volume).
     */
    const val TELEMETRY_BROKER_URL = "http://54.238.185.205:8080/"

    /**
     * Ordered discovery candidates, tried in order until one returns relays (see
     * [com.openrung.client.net.BrokerClient.firstReachable]): the Cloudflare-fronted endpoint first,
     * then the raw IP as a fallback so a blocked edge never takes discovery offline. The raw cleartext
     * IP is why `usesCleartextTraffic` is still enabled in the manifest.
     */
    val DEFAULT_BROKER_URLS: List<String> = listOf(DEFAULT_BROKER_URL, TELEMETRY_BROKER_URL)

    /**
     * Ordered discovery candidates for a connection attempt: the caller-selected [primary] (a user
     * override or the persisted choice) first, then the built-in [DEFAULT_BROKER_URLS], de-duplicated
     * while preserving order. The primary is never discarded, so a user's custom broker is always
     * tried first and the defaults only act as a fallback.
     */
    fun brokerCandidates(primary: String?): List<String> =
        com.openrung.client.net.BrokerClient.candidates(primary, DEFAULT_BROKER_URLS)

    const val RELAY_LIMIT = 5
    const val VPN_SESSION_NAME = "OpenRung Volunteer VPN"
    const val STATUS_PREFS = "openrung_status"

    /**
     * Relay fetch used to populate the exit-node map directory (the connect path still uses
     * [RELAY_LIMIT]). This is the broker's maximum allowed page size — the broker rejects anything
     * larger with HTTP 400 — so it captures the full set of currently-advertised relays.
     */
    const val DIRECTORY_RELAY_LIMIT = 20

    /** Most-recently connected locations kept for the main-screen "Recents" row. */
    const val MAX_RECENTS = 8

    /**
     * Public source repository. Surfaced in the in-app open-source licenses screen and used as the
     * GPL-3.0 corresponding-source offer for the (GPL-licensed) app.
     */
    const val SOURCE_URL = "https://github.com/openrung/openrung"

    /**
     * Vector tiles + glyphs for the exit-node map. We build our own flat style (blue ocean / grey
     * land) around these MapLibre demo tiles rather than using the demo *style*, which colour-codes
     * every country. An operator can point these at a self-hosted source to avoid third-party tiles.
     */
    const val MAP_TILES_URL = "https://demotiles.maplibre.org/tiles/tiles.json"
    const val MAP_GLYPHS_URL = "https://demotiles.maplibre.org/font/{fontstack}/{range}.pbf"
}
