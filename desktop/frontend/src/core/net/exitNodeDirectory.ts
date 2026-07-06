/**
 * PORTED VERBATIM from openrung-mobile-app/src/net/exitNodeDirectory.ts (no changes).
 * Pure TypeScript with an injected fetch, so it is shared as-is with the mobile
 * app. On desktop the injected `fetchRelays` calls the Go binding (which owns
 * broker failover / 429 backoff) rather than the TS broker client.
 *
 * Builds the exit-node map directory from the broker's relay list. The broker serves each
 * relay's exit location (city/country/coordinates, resolved server-side — docs/api.md "List
 * Relays"); the app never geolocates relay IPs itself, both because the broker is the source
 * of truth and because a tunnel (CGNAT) relay's public_host is the relay hub, not where its
 * traffic exits.
 *
 * Usable relays that carry a location are grouped into one `ExitNodeRegion` per distinct
 * country + city, placed at the broker's coordinates (relays in the same city share the
 * broker's city-level coordinate). Relays the broker has not geolocated yet — an older broker,
 * or a lookup that hasn't resolved — simply stay off the map; no position is invented for them.
 *
 * The relay fetch is injected so this stays free of network dependencies and is unit-testable.
 */
import { displayName } from '../model/countryGeo';
import type { ExitNodeRegion } from '../model/exitNode';
import { orderedCandidates, serverTimeMs } from '../model/relay';
import type { RelayDescriptor, RelayListResponse } from '../model/relay';

export interface ExitNodeDirectoryOptions {
  fetchRelays: () => Promise<RelayListResponse>;
}

type LocatedRelay = RelayDescriptor & { country_code: string; latitude: number; longitude: number };

/** A relay the broker has geolocated: coordinates plus the country code tap-to-connect needs. */
function isLocated(relay: RelayDescriptor): relay is LocatedRelay {
  return (
    typeof relay.latitude === 'number' &&
    typeof relay.longitude === 'number' &&
    typeof relay.country_code === 'string' &&
    relay.country_code.trim().length > 0
  );
}

export async function loadExitNodeDirectory(
  options: ExitNodeDirectoryOptions,
): Promise<ExitNodeRegion[]> {
  const response = await options.fetchRelays();
  // Freshness is judged against BROKER server time, never the device clock.
  const usable = orderedCandidates(response.relays, serverTimeMs(response));

  const regionsByKey = new Map<string, ExitNodeRegion>();
  for (const relay of usable) {
    if (!isLocated(relay)) {
      continue;
    }
    const code = relay.country_code.trim().toUpperCase();
    const city = (relay.city ?? '').trim();
    const key = `${code}|${city}`;
    const pickerEntry = {
      id: relay.id,
      label: (relay.label ?? '').trim() || null,
    };
    const existing = regionsByKey.get(key);
    if (existing) {
      existing.nodeCount += 1;
      existing.relays.push(pickerEntry);
      continue;
    }
    const country = (relay.country ?? '').trim();
    regionsByKey.set(key, {
      countryCode: code,
      countryName: country.length > 0 ? country : (displayName(code) ?? code),
      city: city.length > 0 ? city : null,
      latitude: relay.latitude,
      longitude: relay.longitude,
      nodeCount: 1,
      relays: [pickerEntry],
    });
  }

  // Sort by node count desc, then country/city asc (plain code-unit compare, like Kotlin's thenBy).
  const regions = [...regionsByKey.values()];
  regions.sort((a, b) => {
    if (a.nodeCount !== b.nodeCount) {
      return b.nodeCount - a.nodeCount;
    }
    if (a.countryName !== b.countryName) {
      return a.countryName < b.countryName ? -1 : 1;
    }
    const cityA = a.city ?? '';
    const cityB = b.city ?? '';
    return cityA < cityB ? -1 : cityA > cityB ? 1 : 0;
  });
  return regions;
}
