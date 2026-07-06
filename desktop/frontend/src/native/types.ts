// PORTED VERBATIM from openrung-mobile-app/src/native/types.ts — the single
// source of truth for the bridge contract (docs/CONTRACT.md §3). The desktop
// Go service (desktop/vpnservice) implements this same shape, so the mobile
// state layer (store.ts / useVpnState.ts) ports across unchanged.

export type ConnectionStatus =
  | 'disconnected'
  | 'preparing'
  | 'connecting'
  | 'connected'
  | 'disconnecting'
  | 'failed';

export interface RecentNode {
  countryCode: string; // ISO 3166-1 alpha-2, uppercase
  label: string; // "City, Country" or country name
  latitude: number;
  longitude: number;
}

export interface NativeVpnState {
  status: ConnectionStatus;
  relayLabel: string | null; // resolved geo label, never a raw IP
  lastError: string | null;
  logLines: string[]; // "[HH:mm:ss] message", newest last, cap 80
  recents: RecentNode[]; // newest first, deduped by countryCode, cap 8
}

export interface NativeIdentity {
  clientId: string; // stable install UUID (native-persisted)
  sessionId: string | null; // active telemetry session id, null when idle
}

export interface OpenRungVpnModule {
  /** Ask for OS VPN consent. Desktop proxy mode needs none and resolves true;
   *  TUN mode performs the elevation handshake and resolves whether granted. */
  prepare(): Promise<boolean>;
  /** Start (or switch) the tunnel. targetCountry: ISO alpha-2 or null = broker
   *  picks. targetRelayId: connect to that exact broker relay id (takes
   *  precedence over targetCountry) or null. Resolves once the native start
   *  has been dispatched (NOT when connected — completion is reported via
   *  events). */
  connect(
    brokerUrl: string,
    targetCountry: string | null,
    targetRelayId: string | null,
  ): Promise<void>;
  disconnect(): Promise<void>;
  getState(): Promise<NativeVpnState>;
  getIdentity(): Promise<NativeIdentity>;
}
