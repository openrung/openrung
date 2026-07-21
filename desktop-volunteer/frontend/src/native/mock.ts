// Scripted volunteer-relay simulator used when the Wails runtime is absent (a
// plain browser preview via `vite dev`, or vitest). Mirrors the sibling
// desktop/frontend/src/native/mock.ts in spirit: it drives the VolunteerState
// contract through a believable start/stop sequence with growing counters. It
// never touches the network or the OS.
import type {
  VolunteerModule,
  VolunteerSettings,
  VolunteerState,
} from './types';

const ADJECTIVES = ['amber', 'brisk', 'cedar', 'ember', 'harbor', 'lunar', 'mossy', 'quiet'];
const NOUNS = ['lantern', 'otter', 'pine', 'quill', 'ridge', 'sparrow', 'tide', 'willow'];

function randomLabel(): string {
  const adjective = ADJECTIVES[Math.floor(Math.random() * ADJECTIVES.length)];
  const noun = NOUNS[Math.floor(Math.random() * NOUNS.length)];
  const digits = String(Math.floor(Math.random() * 900) + 100);
  return `${adjective}-${noun}-${digits}`;
}

function defaultSettings(): VolunteerSettings {
  return {
    label: randomLabel(),
    maxSessions: 75,
    maxMbps: 100,
    listenPort: 8443,
    brokerUrl: 'https://broker.openrung.org/',
    hubAddress: '',
    connectionMode: 'automatic',
  };
}

export class MockVolunteerService implements VolunteerModule {
  private state: VolunteerState = {
    phase: 'idle',
    transport: '',
    relayLabel: '',
    relayId: '',
    publicEndpoint: '',
    lastError: null,
    startedAtMs: 0,
    activeConnections: 0,
    totalConnections: 0,
    bytesFromClients: 0,
    bytesToClients: 0,
    logLines: [],
    consentAccepted: false,
    running: false,
    xrayFound: true,
    settings: defaultSettings(),
  };
  private readonly listeners = new Set<(s: VolunteerState) => void>();
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();
  private ticker: ReturnType<typeof setInterval> | null = null;

  subscribe(cb: (s: VolunteerState) => void): () => void {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  private stamp(message: string): string {
    const t = new Date().toTimeString().slice(0, 8);
    return `[${t}] ${message}`;
  }

  private emit(patch: Partial<VolunteerState>, log?: string): void {
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

  private clearTimers(): void {
    for (const id of this.timers) clearTimeout(id);
    this.timers.clear();
    if (this.ticker != null) {
      clearInterval(this.ticker);
      this.ticker = null;
    }
  }

  /** One second of simulated relay traffic while online. */
  private tick(): void {
    const active = Math.max(
      0,
      Math.min(this.state.settings.maxSessions, this.state.activeConnections + Math.floor(Math.random() * 3) - 1),
    );
    const arrivals = Math.max(0, active - this.state.activeConnections);
    this.emit({
      activeConnections: active,
      totalConnections: this.state.totalConnections + arrivals,
      bytesFromClients: this.state.bytesFromClients + Math.floor(Math.random() * 300_000) + 20_000,
      bytesToClients: this.state.bytesToClients + Math.floor(Math.random() * 1_200_000) + 60_000,
    });
  }

  async getState(): Promise<VolunteerState> {
    return this.state;
  }

  async start(): Promise<void> {
    if (!this.state.consentAccepted) {
      throw new Error('consent required before volunteering');
    }
    if (!this.state.xrayFound) {
      throw new Error('xray engine not found');
    }
    if (this.state.running) {
      return;
    }
    this.clearTimers();

    this.emit(
      {
        phase: 'starting',
        running: true,
        lastError: null,
        transport: '',
        relayLabel: '',
        relayId: '',
        publicEndpoint: '',
        activeConnections: 0,
        totalConnections: 0,
        bytesFromClients: 0,
        bytesToClients: 0,
      },
      'starting xray engine',
    );
    this.later(() => this.emit({ phase: 'probing' }, 'probing public reachability'), 500);
    this.later(() => this.emit({ phase: 'registering' }, 'registering with broker'), 1400);
    this.later(() => {
      const endpoint = `203.0.113.20:${this.state.settings.listenPort}`;
      this.emit(
        {
          phase: 'online',
          transport: 'direct',
          relayLabel: this.state.settings.label,
          relayId: 'mock-relay-0001',
          publicEndpoint: endpoint,
          startedAtMs: Date.now(),
        },
        `online as ${this.state.settings.label} (${endpoint})`,
      );
      this.ticker = setInterval(() => this.tick(), 1000);
    }, 2100);
  }

  async stop(): Promise<void> {
    this.clearTimers();
    this.emit({ phase: 'stopping' }, 'stopping');
    this.later(
      () =>
        this.emit(
          {
            phase: 'idle',
            running: false,
            transport: '',
            relayLabel: '',
            relayId: '',
            publicEndpoint: '',
            startedAtMs: 0,
            activeConnections: 0,
          },
          'stopped',
        ),
      350,
    );
  }

  async getSettings(): Promise<VolunteerSettings> {
    return this.state.settings;
  }

  // Mirrors the Go service's SaveSettings validation and normalization.
  async saveSettings(input: VolunteerSettings): Promise<VolunteerSettings> {
    if (input.maxSessions < 1 || input.maxSessions > 4096) {
      throw new Error('max sessions must be between 1 and 4096');
    }
    if (input.maxMbps < 1 || input.maxMbps > 10000) {
      throw new Error('max Mbps must be between 1 and 10000');
    }
    if (input.listenPort < 1 || input.listenPort > 65535) {
      throw new Error('listen port must be between 1 and 65535');
    }
    const settings: VolunteerSettings = {
      label: input.label.trim() || randomLabel(),
      maxSessions: Math.floor(input.maxSessions),
      maxMbps: Math.floor(input.maxMbps),
      listenPort: Math.floor(input.listenPort),
      brokerUrl: input.brokerUrl.trim() || 'https://broker.openrung.org/',
      hubAddress: input.hubAddress.trim(),
      connectionMode: input.connectionMode === 'direct' ? 'direct' : 'automatic',
    };
    this.emit({ settings }, 'settings saved');
    return settings;
  }

  async regenerateLabel(): Promise<string> {
    const label = randomLabel();
    this.emit({ settings: { ...this.state.settings, label } }, `relay name is now ${label}`);
    return label;
  }

  async acceptConsent(): Promise<void> {
    this.emit({ consentAccepted: true }, 'volunteer consent accepted');
  }

  async running(): Promise<boolean> {
    return this.state.running;
  }
}
