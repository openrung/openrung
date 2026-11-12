// Fixtures for the browser preview only (no Wails runtime, so no Go service).
// The relay list uses the same placeholder locations and documentation-range
// hosts as the existing desktop frontend's mock; none of this is real data.
import type { WailsProxyInfo, WailsRelayListResponse, WailsTunnelMode } from './wails';

export function previewProxyInfo(): WailsProxyInfo {
  return {
    endpoint: '127.0.0.1:18080',
    persistenceWarning: null,
    shellIntegration: true,
    shellIntegrationError: null,
    enableCommand: '. "$HOME/.config/openrung/proxy.sh" && openrung_proxy_on',
    disableCommand: 'openrung_proxy_off',
  };
}

/**
 * A stand-in for the host's capture mode, so the preview shows the TUN mode
 * switch and what Settings does around it. Nothing restarts and nothing is
 * captured: the switch only flips a value held here.
 */
export function previewTunnelMode() {
  const state: WailsTunnelMode = { mode: 'proxy', available: true, requiresRestart: true };
  return {
    getTunnelMode: async (): Promise<WailsTunnelMode> => ({ ...state }),
    setTunnelMode: async (mode: string): Promise<void> => {
      state.mode = mode === 'tun' ? 'tun' : 'proxy';
    },
  };
}

const SAMPLE_LOCATIONS = [
  { code: 'JP', country: 'Japan', city: 'Tokyo', lat: 35.68, lng: 139.69 },
  { code: 'JP', country: 'Japan', city: 'Osaka', lat: 34.69, lng: 135.5 },
  { code: 'SG', country: 'Singapore', city: 'Singapore', lat: 1.35, lng: 103.82 },
  { code: 'US', country: 'United States', city: 'Los Angeles', lat: 34.05, lng: -118.24 },
  { code: 'DE', country: 'Germany', city: 'Frankfurt', lat: 50.11, lng: 8.68 },
  { code: 'NL', country: 'Netherlands', city: 'Amsterdam', lat: 52.37, lng: 4.9 },
  { code: 'KR', country: 'South Korea', city: 'Seoul', lat: 37.57, lng: 126.98 },
];

export function previewRelayList(): WailsRelayListResponse {
  const now = Date.now();
  const relays = SAMPLE_LOCATIONS.map((loc, index) => ({
    id: `mock-${loc.code}-${index}`,
    label: `mock-${loc.city.toLowerCase().replace(/\s+/g, '-')}`,
    public_host: `203.0.113.${10 + index}`,
    public_port: 443,
    protocol: 'vless-reality-vision',
    client_id: '00000000-0000-4000-8000-000000000000',
    reality_public_key: 'mock-reality-public-key',
    short_id: '0123abcd',
    server_name: 'www.example.com',
    flow: 'xtls-rprx-vision',
    exit_mode: 'direct',
    max_sessions: 32,
    max_mbps: 100,
    node_class: index % 2 === 0 ? 'foundation' : 'volunteer',
    volunteer_version: '0.0.0-mock',
    registered_at: new Date(now - 3_600_000).toISOString(),
    last_heartbeat_at: new Date(now).toISOString(),
    expires_at: new Date(now + 3_600_000).toISOString(),
    city: loc.city,
    country: loc.country,
    country_code: loc.code,
    latitude: loc.lat,
    longitude: loc.lng,
  }));
  return { count: relays.length, server_time: new Date(now).toISOString(), relays };
}
