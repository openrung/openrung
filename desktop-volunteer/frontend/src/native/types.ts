// Bridge contract for the volunteer desktop app. The Go service
// (desktop-volunteer/volunteerservice/service.go) is the source of truth for
// these shapes; every field name matches its JSON tags exactly, so the Wails
// bindings and the scripted mock serve the identical payload.

export type VolunteerPhase =
  | 'idle'
  | 'starting'
  | 'probing'
  | 'registering'
  | 'online'
  | 'retrying'
  | 'stopping';

export type VolunteerTransport = '' | 'direct' | 'tunnel';

export interface VolunteerSettings {
  label: string; // public relay name shown in the directory
  maxSessions: number; // advertised capacity
  maxMbps: number; // advertised speed target (NOT strictly enforced yet)
  listenPort: number; // direct-mode port (advanced)
  brokerUrl: string; // advanced
  hubAddress: string; // advanced, host:port, empty = no hub
}

export interface VolunteerState {
  phase: VolunteerPhase;
  transport: VolunteerTransport;
  relayLabel: string;
  relayId: string;
  publicEndpoint: string; // "host:port" or ""
  lastError: string | null;
  startedAtMs: number; // 0 when not online
  activeConnections: number;
  totalConnections: number;
  bytesFromClients: number;
  bytesToClients: number;
  logLines: string[]; // "[HH:mm:ss] message", newest last
  consentAccepted: boolean;
  running: boolean;
  xrayFound: boolean;
  settings: VolunteerSettings;
}

export interface VolunteerModule {
  getState(): Promise<VolunteerState>;
  /** Rejects when consent has not been accepted or the xray engine is missing. */
  start(): Promise<void>;
  stop(): Promise<void>;
  getSettings(): Promise<VolunteerSettings>;
  /** Rejects with a human-readable validation message on bad input. */
  saveSettings(settings: VolunteerSettings): Promise<VolunteerSettings>;
  regenerateLabel(): Promise<string>;
  acceptConsent(): Promise<void>;
  running(): Promise<boolean>;
}
