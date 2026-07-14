/**
 * PORTED VERBATIM from openrung-mobile-app/src/model/countryGeo.ts (no changes).
 * Pure TypeScript, shared as-is with the mobile app. On desktop it also backs
 * the static SVG fallback map used when WebGL is unavailable.
 *
 * Curated ISO-3166 alpha-2 -> centroid table used to place exit-node markers on the map without a
 * per-host geocoding round trip. Asia-Pacific is covered densely (the relay network's focus);
 * common VPN-exit countries elsewhere are included so a stray relay still lands somewhere sensible.
 *
 * Coordinates are approximate country centroids (latitude, longitude in degrees).
 *
 * Ported verbatim from the production `model/CountryGeo.kt`.
 */

export interface Centroid {
  name: string;
  latitude: number;
  longitude: number;
}

const table: Record<string, Centroid> = {
  // East Asia
  JP: { name: 'Japan', latitude: 36.2, longitude: 138.25 },
  KR: { name: 'South Korea', latitude: 36.5, longitude: 127.85 },
  KP: { name: 'North Korea', latitude: 40.34, longitude: 127.51 },
  CN: { name: 'China', latitude: 35.86, longitude: 104.2 },
  HK: { name: 'Hong Kong', latitude: 22.32, longitude: 114.17 },
  MO: { name: 'Macau', latitude: 22.2, longitude: 113.55 },
  TW: { name: 'Taiwan', latitude: 23.7, longitude: 121.0 },
  MN: { name: 'Mongolia', latitude: 46.86, longitude: 103.85 },
  // Southeast Asia
  SG: { name: 'Singapore', latitude: 1.35, longitude: 103.82 },
  MY: { name: 'Malaysia', latitude: 4.21, longitude: 101.98 },
  ID: { name: 'Indonesia', latitude: -2.5, longitude: 118.0 },
  TH: { name: 'Thailand', latitude: 15.87, longitude: 100.99 },
  VN: { name: 'Vietnam', latitude: 14.06, longitude: 108.28 },
  PH: { name: 'Philippines', latitude: 12.88, longitude: 121.77 },
  KH: { name: 'Cambodia', latitude: 12.57, longitude: 104.99 },
  LA: { name: 'Laos', latitude: 19.86, longitude: 102.5 },
  MM: { name: 'Myanmar', latitude: 21.91, longitude: 95.96 },
  BN: { name: 'Brunei', latitude: 4.54, longitude: 114.73 },
  TL: { name: 'Timor-Leste', latitude: -8.87, longitude: 125.73 },
  // South Asia
  IN: { name: 'India', latitude: 22.0, longitude: 79.0 },
  BD: { name: 'Bangladesh', latitude: 23.68, longitude: 90.36 },
  PK: { name: 'Pakistan', latitude: 30.38, longitude: 69.35 },
  LK: { name: 'Sri Lanka', latitude: 7.87, longitude: 80.77 },
  NP: { name: 'Nepal', latitude: 28.39, longitude: 84.12 },
  BT: { name: 'Bhutan', latitude: 27.51, longitude: 90.43 },
  MV: { name: 'Maldives', latitude: 3.2, longitude: 73.22 },
  AF: { name: 'Afghanistan', latitude: 33.94, longitude: 67.71 },
  // Oceania
  AU: { name: 'Australia', latitude: -25.27, longitude: 133.78 },
  NZ: { name: 'New Zealand', latitude: -41.0, longitude: 174.0 },
  FJ: { name: 'Fiji', latitude: -17.71, longitude: 178.07 },
  PG: { name: 'Papua New Guinea', latitude: -6.31, longitude: 143.96 },
  // Central Asia
  KZ: { name: 'Kazakhstan', latitude: 48.02, longitude: 66.92 },
  UZ: { name: 'Uzbekistan', latitude: 41.38, longitude: 64.59 },
  KG: { name: 'Kyrgyzstan', latitude: 41.2, longitude: 74.77 },
  TJ: { name: 'Tajikistan', latitude: 38.86, longitude: 71.28 },
  TM: { name: 'Turkmenistan', latitude: 38.97, longitude: 59.56 },
  // West Asia / Middle East
  AE: { name: 'United Arab Emirates', latitude: 23.42, longitude: 53.85 },
  SA: { name: 'Saudi Arabia', latitude: 23.89, longitude: 45.08 },
  TR: { name: 'Turkey', latitude: 38.96, longitude: 35.24 },
  IR: { name: 'Iran', latitude: 32.43, longitude: 53.69 },
  IL: { name: 'Israel', latitude: 31.05, longitude: 34.85 },
  QA: { name: 'Qatar', latitude: 25.35, longitude: 51.18 },
  // Common exit countries outside APAC
  RU: { name: 'Russia', latitude: 61.52, longitude: 105.32 },
  US: { name: 'United States', latitude: 39.0, longitude: -98.0 },
  CA: { name: 'Canada', latitude: 56.13, longitude: -106.35 },
  GB: { name: 'United Kingdom', latitude: 55.38, longitude: -3.44 },
  DE: { name: 'Germany', latitude: 51.17, longitude: 10.45 },
  NL: { name: 'Netherlands', latitude: 52.13, longitude: 5.29 },
  FR: { name: 'France', latitude: 46.6, longitude: 2.45 },
  FI: { name: 'Finland', latitude: 61.92, longitude: 25.75 },
  SE: { name: 'Sweden', latitude: 60.13, longitude: 18.64 },
};

/** Number of entries in the curated table (exposed for tests). */
export const COUNTRY_GEO_SIZE = Object.keys(table).length;

/** Centroid for an ISO 3166-1 alpha-2 country code (case-insensitive), or null if unknown. */
export function centroid(countryCode: string): Centroid | null {
  return table[countryCode.trim().toUpperCase()] ?? null;
}

/** Display name for a country code, or null if unknown. */
export function displayName(countryCode: string): string | null {
  return centroid(countryCode)?.name ?? null;
}
