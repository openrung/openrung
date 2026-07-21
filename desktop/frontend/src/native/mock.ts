// Scripted VPN simulator used when the Wails runtime is absent (a plain browser
// preview via `vite dev`, or vitest). Mirrors openrung-mobile-app/src/native/mock.ts
// in spirit: it drives the same NativeVpnState contract through a believable
// connect/disconnect sequence and serves a sample relay list so the map renders
// without a broker. It never touches the network or the OS.
import type {
  NativeIdentity,
  NativeProxyInfo,
  NativeVpnState,
  OpenRungVpnModule,
  RecentNode,
} from './types';
import type { RelayDescriptor, RelayListResponse } from '../core/model/relay';

type Located = { code: string; country: string; city: string; lat: number; lng: number };

const SAMPLE_LOCATIONS: Located[] = [
  { code: 'JP', country: 'Japan', city: 'Tokyo', lat: 35.68, lng: 139.69 },
  { code: 'JP', country: 'Japan', city: 'Osaka', lat: 34.69, lng: 135.5 },
  { code: 'SG', country: 'Singapore', city: 'Singapore', lat: 1.35, lng: 103.82 },
  { code: 'US', country: 'United States', city: 'Los Angeles', lat: 34.05, lng: -118.24 },
  { code: 'DE', country: 'Germany', city: 'Frankfurt', lat: 50.11, lng: 8.68 },
  { code: 'NL', country: 'Netherlands', city: 'Amsterdam', lat: 52.37, lng: 4.9 },
  { code: 'KR', country: 'South Korea', city: 'Seoul', lat: 37.57, lng: 126.98 },
];

function sampleRelay(loc: Located, index: number): RelayDescriptor {
  return {
    id: `mock-${loc.code}-${index}`,
    label: `mock-${loc.city.toLowerCase()}`,
    public_host: `203.0.113.${10 + index}`,
    public_port: 443,
    protocol: 'vless-reality-vision',
    client_id: '00000000-0000-4000-8000-000000000000',
    reality_public_key: 'mock-reality-public-key',
    short_id: '0123abcd',
    server_name: 'www.cloudflare.com',
    flow: 'xtls-rprx-vision',
    exit_mode: 'direct',
    max_sessions: 32,
    max_mbps: 100,
    relay_version: '0.0.0-mock',
    registered_at: new Date(Date.now() - 3_600_000).toISOString(),
    last_heartbeat_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 3_600_000).toISOString(),
    city: loc.city,
    country: loc.country,
    country_code: loc.code,
    latitude: loc.lat,
    longitude: loc.lng,
  };
}

export class MockOpenRungVpn implements OpenRungVpnModule {
  private state: NativeVpnState = {
    status: 'disconnected',
    relayLabel: null,
    lastError: null,
    logLines: [],
    recents: [],
  };
  private readonly listeners = new Set<(s: NativeVpnState) => void>();
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();

  subscribe(cb: (s: NativeVpnState) => void): () => void {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  private stamp(message: string): string {
    const t = new Date().toTimeString().slice(0, 8);
    return `[${t}] ${message}`;
  }

  private emit(patch: Partial<NativeVpnState>, log?: string): void {
    const logLines = log
      ? [...this.state.logLines, this.stamp(log)].slice(-80)
      : this.state.logLines;
    this.state = { ...this.state, ...patch, logLines };
    for (const cb of this.listeners) {
      cb(this.state);
    }
  }

  private later(fn: () => void, ms: number): void {
    const id = setTimeout(() => {
      this.timers.delete(id);
      fn();
    }, ms);
    this.timers.add(id);
  }

  async prepare(): Promise<boolean> {
    return true;
  }

  async connect(_brokerUrl: string, targetCountry: string | null): Promise<void> {
    for (const id of this.timers) clearTimeout(id);
    this.timers.clear();

    const loc =
      SAMPLE_LOCATIONS.find(l => l.code === (targetCountry ?? '').toUpperCase()) ??
      SAMPLE_LOCATIONS[0];
    const label = `${loc.city}, ${loc.country}`;

    this.emit({ status: 'connecting', lastError: null }, `connecting to ${label}`);
    this.later(() => this.emit({}, 'handshake: reality + vision ok'), 500);
    this.later(() => {
      const recent: RecentNode = {
        countryCode: loc.code,
        label,
        latitude: loc.lat,
        longitude: loc.lng,
      };
      const recents = [recent, ...this.state.recents.filter(r => r.countryCode !== loc.code)].slice(
        0,
        8,
      );
      this.emit({ status: 'connected', relayLabel: label, recents }, `connected via ${label}`);
    }, 1000);
  }

  async disconnect(): Promise<void> {
    for (const id of this.timers) clearTimeout(id);
    this.timers.clear();
    this.emit({ status: 'disconnecting' }, 'disconnecting');
    this.later(() => this.emit({ status: 'disconnected', relayLabel: null }, 'disconnected'), 350);
  }

  async getState(): Promise<NativeVpnState> {
    return this.state;
  }

  async getIdentity(): Promise<NativeIdentity> {
    return { clientId: 'mock-client-0000', sessionId: null };
  }

  async getProxyInfo(): Promise<NativeProxyInfo> {
    return {
      host: '127.0.0.1',
      port: 46685,
      endpoint: '127.0.0.1:46685',
      persistenceWarning: null,
      shellIntegration: true,
      shellIntegrationError: null,
      helperPath: '$HOME/.config/openrung/proxy-env.sh',
      enableCommand: '. "$HOME/.config/openrung/proxy-env.sh" && openrung_proxy_on',
      disableCommand: 'openrung_proxy_off',
    };
  }

  async listRelaysForDirectory(): Promise<RelayListResponse> {
    const relays = SAMPLE_LOCATIONS.map(sampleRelay);
    return {
      count: relays.length,
      server_time: new Date().toISOString(),
      relays,
    };
  }
}
