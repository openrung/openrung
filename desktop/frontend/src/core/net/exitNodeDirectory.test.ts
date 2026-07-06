// Ported from openrung-mobile-app/__tests__/core/exitNodeDirectory.test.ts
// (Jest → vitest). Verifies grouping-by-country+city, node counts, ordering,
// and that ungeolocated relays stay off the map.
import { describe, expect, it } from 'vitest';
import { loadExitNodeDirectory } from './exitNodeDirectory';
import type { RelayDescriptor, RelayListResponse } from '../model/relay';

function relay(overrides: Partial<RelayDescriptor>): RelayDescriptor {
  return {
    id: Math.random().toString(36).slice(2),
    public_host: '1.2.3.4',
    public_port: 443,
    protocol: 'vless-reality-vision',
    client_id: 'uuid',
    reality_public_key: 'pk',
    short_id: 'sid',
    server_name: 'sni',
    flow: 'xtls-rprx-vision',
    exit_mode: 'direct',
    max_sessions: 8,
    max_mbps: 100,
    volunteer_version: '1',
    registered_at: '2026-07-05T00:00:00Z',
    last_heartbeat_at: '2026-07-06T00:00:00Z',
    expires_at: '2026-07-06T01:00:00Z',
    ...overrides,
  };
}

function response(relays: RelayDescriptor[]): RelayListResponse {
  return { count: relays.length, server_time: '2026-07-06T00:00:00Z', relays };
}

describe('loadExitNodeDirectory', () => {
  it('groups usable located relays by country+city and counts nodes', async () => {
    const regions = await loadExitNodeDirectory({
      fetchRelays: async () =>
        response([
          relay({ country_code: 'jp', country: 'Japan', city: 'Tokyo', latitude: 35.6, longitude: 139.7 }),
          relay({ country_code: 'JP', country: 'Japan', city: 'Tokyo', latitude: 35.6, longitude: 139.7 }),
          relay({ country_code: 'SG', country: 'Singapore', city: 'Singapore', latitude: 1.35, longitude: 103.8 }),
        ]),
    });

    expect(regions).toHaveLength(2);
    // Sorted by node count desc: Tokyo (2) before Singapore (1).
    expect(regions[0]).toMatchObject({ countryCode: 'JP', city: 'Tokyo', nodeCount: 2 });
    expect(regions[0].relays).toHaveLength(2);
    expect(regions[1]).toMatchObject({ countryCode: 'SG', nodeCount: 1 });
  });

  it('drops relays the broker has not geolocated', async () => {
    const regions = await loadExitNodeDirectory({
      fetchRelays: async () => response([relay({ country_code: undefined, latitude: undefined })]),
    });
    expect(regions).toHaveLength(0);
  });

  it('excludes unusable relays before grouping', async () => {
    const regions = await loadExitNodeDirectory({
      fetchRelays: async () =>
        response([
          relay({ protocol: 'other', country_code: 'JP', latitude: 35.6, longitude: 139.7 }),
        ]),
    });
    expect(regions).toHaveLength(0);
  });
});
