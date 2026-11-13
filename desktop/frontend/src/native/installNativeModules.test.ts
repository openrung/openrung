import { DeviceEventEmitter, Linking, NativeModules } from 'react-native-web';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { installNativeModules } from './installNativeModules';

interface BrokerModule {
  firstReachable(requestId: string, primary: string): Promise<Record<string, unknown>>;
  fetchManifestCandidate(requestId: string, candidateUrl: string): Promise<unknown>;
  sendTelemetryBatchJSON(): Promise<unknown>;
}
interface VpnModule {
  connect(brokerUrl: string, country: string | null, relayId: string | null): Promise<void>;
  getState(): Promise<unknown>;
  setSplitTunnelConfig(config: string): Promise<void>;
}
interface DesktopModule {
  appVersion: string;
  getProxyInfo(): Promise<{ endpoint: string }>;
  copyText(text: string): Promise<void>;
  getTunnelMode(): Promise<{ mode: string; available: boolean; requiresRestart: boolean }>;
  setTunnelMode(mode: string): Promise<void>;
}

const modules = NativeModules as {
  OpenRungVpn?: VpnModule;
  OpenRungBroker?: BrokerModule;
  OpenRungDesktop?: DesktopModule;
};
const originalOpenURL = Linking.openURL;

function fakeWails() {
  const listeners: Array<(state: unknown) => void> = [];
  const service = {
    Prepare: vi.fn(async () => true),
    Connect: vi.fn(async () => undefined),
    Disconnect: vi.fn(async () => undefined),
    GetState: vi.fn(async () => ({ status: 'disconnected' })),
    GetIdentity: vi.fn(async () => ({ clientId: 'client-1', sessionId: null })),
    SetSplitTunnelConfig: vi.fn(async () => undefined),
    GetProxyInfo: vi.fn(async () => ({
      endpoint: '127.0.0.1:4000',
      persistenceWarning: null,
      shellIntegration: false,
      shellIntegrationError: null,
      enableCommand: '',
      disableCommand: '',
    })),
    GetTunnelMode: vi.fn(async () => ({
      mode: 'proxy' as const,
      available: true,
      requiresRestart: true,
    })),
    SetTunnelMode: vi.fn(async () => undefined),
    FetchManifestCandidate: vi.fn(async (candidateUrl: string) => ({
      sourceUrl: candidateUrl,
      bodyJson: '{"schema":1}',
    })),
    ListRelaysForDirectory: vi.fn(async () => ({
      count: 0,
      server_time: '2026-01-01T00:00:00Z',
      relays: [],
      key_id: 'key-1',
    })),
  };
  const runtime = {
    EventsOn: vi.fn((_event: string, callback: (state: unknown) => void) => {
      listeners.push(callback);
      return () => undefined;
    }),
    BrowserOpenURL: vi.fn(),
    ClipboardSetText: vi.fn(async () => true),
  };
  window.go = { vpnservice: { Service: service } };
  window.runtime = runtime;
  return { service, runtime, emit: (state: unknown) => listeners.forEach(l => l(state)) };
}

afterEach(() => {
  delete modules.OpenRungVpn;
  delete modules.OpenRungBroker;
  delete modules.OpenRungDesktop;
  delete window.go;
  delete window.runtime;
  Linking.openURL = originalOpenURL;
});

describe('installNativeModules without the Wails runtime (browser preview)', () => {
  it('leaves OpenRungVpn unset so the app falls back to its own mock', () => {
    installNativeModules();
    expect(modules.OpenRungVpn).toBeUndefined();
  });

  it('serves a fixture directory and proxy endpoint so every screen renders', async () => {
    installNativeModules();
    const fetched = await modules.OpenRungBroker!.firstReachable('req-1', 'https://broker.example/');
    const list = JSON.parse(fetched.relayJson as string) as { relays: unknown[] };
    expect(list.relays.length).toBeGreaterThan(0);
    expect((await modules.OpenRungDesktop!.getProxyInfo()).endpoint).toMatch(/^127\.0\.0\.1:\d+$/);
  });
});

describe('installNativeModules under Wails', () => {
  it('maps the bridge contract onto the Go service bindings', async () => {
    const { service } = fakeWails();
    installNativeModules();

    // The contract's nulls are empty strings on the Go side.
    await modules.OpenRungVpn!.connect('https://broker.example/', null, null);
    expect(service.Connect).toHaveBeenLastCalledWith('https://broker.example/', '', '');
    await modules.OpenRungVpn!.connect('https://broker.example/', 'JP', 'relay_a');
    expect(service.Connect).toHaveBeenLastCalledWith('https://broker.example/', 'JP', 'relay_a');

    expect(await modules.OpenRungVpn!.getState()).toEqual({ status: 'disconnected' });
    const splitConfig = '{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn"]}';
    await modules.OpenRungVpn!.setSplitTunnelConfig(splitConfig);
    expect(service.SetSplitTunnelConfig).toHaveBeenLastCalledWith(splitConfig);
  });

  it('forwards Go state events to the app as the device event it listens for', () => {
    const { emit } = fakeWails();
    installNativeModules();
    const received: unknown[] = [];
    const subscription = DeviceEventEmitter.addListener('openrungStateChanged', state =>
      received.push(state),
    );

    emit({ status: 'connected', relayName: 'alpha' });
    subscription.remove();
    expect(received).toEqual([{ status: 'connected', relayName: 'alpha' }]);
  });

  it('answers the directory fetch from the Go broker client, marked verified', async () => {
    const { service } = fakeWails();
    installNativeModules();

    const fetched = await modules.OpenRungBroker!.firstReachable('req-1', 'https://broker.example/');
    expect(service.ListRelaysForDirectory).toHaveBeenCalledTimes(1);
    expect(fetched).toMatchObject({
      brokerUrl: 'https://broker.example/',
      keyId: 'key-1',
      signatureVerified: true,
    });
    expect(JSON.parse(fetched.relayJson as string)).toMatchObject({ server_time: '2026-01-01T00:00:00Z' });
  });

  it('fetches an update-manifest candidate through the Go service', async () => {
    const { service } = fakeWails();
    installNativeModules();

    const url = 'https://broker.openrung.org/api/v1/app-manifest';
    const fetched = await modules.OpenRungBroker!.fetchManifestCandidate('req-1', url);
    expect(service.FetchManifestCandidate).toHaveBeenCalledWith(url);
    expect(fetched).toEqual({ sourceUrl: url, bodyJson: '{"schema":1}' });
  });

  it('rejects broker features the desktop host does not implement with a coded error', async () => {
    fakeWails();
    installNativeModules();
    await expect(modules.OpenRungBroker!.sendTelemetryBatchJSON()).rejects.toMatchObject({ code: 'unavailable' });
  });

  it('has no update manifest to offer in a browser preview without the host', async () => {
    installNativeModules();
    await expect(
      modules.OpenRungBroker!.fetchManifestCandidate('req-1', 'https://broker.openrung.org/api/v1/app-manifest'),
    ).rejects.toMatchObject({ code: 'unavailable' });
  });

  it('exposes the desktop host: version, proxy details, clipboard and external links', async () => {
    const { service, runtime } = fakeWails();
    installNativeModules();

    expect(modules.OpenRungDesktop!.appVersion).toBe(__APP_VERSION__);
    expect((await modules.OpenRungDesktop!.getProxyInfo()).endpoint).toBe('127.0.0.1:4000');
    expect(service.GetProxyInfo).toHaveBeenCalledTimes(1);

    await modules.OpenRungDesktop!.copyText('127.0.0.1:4000');
    expect(runtime.ClipboardSetText).toHaveBeenCalledWith('127.0.0.1:4000');

    // The capture mode is the host's to report and to change.
    expect(await modules.OpenRungDesktop!.getTunnelMode()).toEqual({
      mode: 'proxy',
      available: true,
      requiresRestart: true,
    });
    await modules.OpenRungDesktop!.setTunnelMode('tun');
    expect(service.SetTunnelMode).toHaveBeenCalledWith('tun');

    // The webview must not navigate itself away from the app.
    await Linking.openURL('https://openrung.org/');
    expect(runtime.BrowserOpenURL).toHaveBeenCalledWith('https://openrung.org/');
  });
});
