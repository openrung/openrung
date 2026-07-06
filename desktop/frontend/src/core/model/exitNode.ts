/**
 * PORTED VERBATIM from openrung-mobile-app/src/model/exitNode.ts (no changes).
 * Pure TypeScript, shared as-is with the mobile app.
 */

/** One volunteer relay inside an [ExitNodeRegion]: just what the picker UI needs. */
export interface ExitNodeRelay {
  id: string; // broker relay id — what connect-to-this-relay targets
  label: string | null; // volunteer-chosen name (e.g. "silly-lemur"); null on older brokers
}

/**
 * One located exit spot on the exit-node map. Volunteer relays are grouped by the broker-served
 * exit location (country + city), so a single marker may stand for several nodes (`nodeCount`,
 * with the individual relays listed in `relays` for the list view's per-relay picker).
 * Coordinates come straight from the broker and are city-level accurate at best — the map must
 * not imply street-level precision.
 */
export interface ExitNodeRegion {
  countryCode: string; // ISO 3166-1 alpha-2, uppercase — what tap-to-connect targets
  countryName: string;
  city: string | null; // null when the broker only knows the country
  latitude: number;
  longitude: number;
  nodeCount: number;
  relays: ExitNodeRelay[]; // broker order; nodeCount === relays.length
}

/** Load state of the exit-node map directory (the list of available exit-node regions). */
export type DirectoryStatus = 'idle' | 'loading' | 'loaded' | 'failed';

/** Presentation of the exit-node directory on the home screen: map backdrop or scrollable list. */
export type HomeViewMode = 'map' | 'list';
