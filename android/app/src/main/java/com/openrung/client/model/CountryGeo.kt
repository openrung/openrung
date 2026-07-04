package com.openrung.client.model

/**
 * Static country centroid + display-name table used to place exit-node markers on the map without a
 * per-host geocoding round trip. Asia-Pacific is covered densely (the volunteer network's focus);
 * common VPN-exit countries elsewhere are included so a stray relay still lands somewhere sensible.
 *
 * Coordinates are approximate country centroids (latitude, longitude in degrees).
 */
object CountryGeo {
    data class Centroid(val name: String, val latitude: Double, val longitude: Double)

    private val table: Map<String, Centroid> = mapOf(
        // East Asia
        "JP" to Centroid("Japan", 36.20, 138.25),
        "KR" to Centroid("South Korea", 36.50, 127.85),
        "KP" to Centroid("North Korea", 40.34, 127.51),
        "CN" to Centroid("China", 35.86, 104.20),
        "HK" to Centroid("Hong Kong", 22.32, 114.17),
        "MO" to Centroid("Macau", 22.20, 113.55),
        "TW" to Centroid("Taiwan", 23.70, 121.00),
        "MN" to Centroid("Mongolia", 46.86, 103.85),
        // Southeast Asia
        "SG" to Centroid("Singapore", 1.35, 103.82),
        "MY" to Centroid("Malaysia", 4.21, 101.98),
        "ID" to Centroid("Indonesia", -2.50, 118.00),
        "TH" to Centroid("Thailand", 15.87, 100.99),
        "VN" to Centroid("Vietnam", 14.06, 108.28),
        "PH" to Centroid("Philippines", 12.88, 121.77),
        "KH" to Centroid("Cambodia", 12.57, 104.99),
        "LA" to Centroid("Laos", 19.86, 102.50),
        "MM" to Centroid("Myanmar", 21.91, 95.96),
        "BN" to Centroid("Brunei", 4.54, 114.73),
        "TL" to Centroid("Timor-Leste", -8.87, 125.73),
        // South Asia
        "IN" to Centroid("India", 22.00, 79.00),
        "BD" to Centroid("Bangladesh", 23.68, 90.36),
        "PK" to Centroid("Pakistan", 30.38, 69.35),
        "LK" to Centroid("Sri Lanka", 7.87, 80.77),
        "NP" to Centroid("Nepal", 28.39, 84.12),
        "BT" to Centroid("Bhutan", 27.51, 90.43),
        "MV" to Centroid("Maldives", 3.20, 73.22),
        "AF" to Centroid("Afghanistan", 33.94, 67.71),
        // Oceania
        "AU" to Centroid("Australia", -25.27, 133.78),
        "NZ" to Centroid("New Zealand", -41.00, 174.00),
        "FJ" to Centroid("Fiji", -17.71, 178.07),
        "PG" to Centroid("Papua New Guinea", -6.31, 143.96),
        // Central Asia
        "KZ" to Centroid("Kazakhstan", 48.02, 66.92),
        "UZ" to Centroid("Uzbekistan", 41.38, 64.59),
        "KG" to Centroid("Kyrgyzstan", 41.20, 74.77),
        "TJ" to Centroid("Tajikistan", 38.86, 71.28),
        "TM" to Centroid("Turkmenistan", 38.97, 59.56),
        // West Asia / Middle East
        "AE" to Centroid("United Arab Emirates", 23.42, 53.85),
        "SA" to Centroid("Saudi Arabia", 23.89, 45.08),
        "TR" to Centroid("Turkey", 38.96, 35.24),
        "IR" to Centroid("Iran", 32.43, 53.69),
        "IL" to Centroid("Israel", 31.05, 34.85),
        "QA" to Centroid("Qatar", 25.35, 51.18),
        // Common exit countries outside APAC
        "RU" to Centroid("Russia", 61.52, 105.32),
        "US" to Centroid("United States", 39.00, -98.00),
        "CA" to Centroid("Canada", 56.13, -106.35),
        "GB" to Centroid("United Kingdom", 55.38, -3.44),
        "DE" to Centroid("Germany", 51.17, 10.45),
        "NL" to Centroid("Netherlands", 52.13, 5.29),
        "FR" to Centroid("France", 46.60, 2.45),
        "FI" to Centroid("Finland", 61.92, 25.75),
        "SE" to Centroid("Sweden", 60.13, 18.64),
    )

    /** Centroid for an ISO 3166-1 alpha-2 [countryCode] (case-insensitive), or null if unknown. */
    fun centroid(countryCode: String): Centroid? = table[countryCode.trim().uppercase()]

    /** Display name for [countryCode], or null if unknown. */
    fun displayName(countryCode: String): String? = centroid(countryCode)?.name
}
