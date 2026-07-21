// Desktop adapter over the Wails-bound Go VPNService. It maps the Go bindings
// (window.go.vpnservice.Service.*, capitalized Go method names) and the
// openrungStateChanged runtime event onto the OpenRungVpnModule contract, so
// the ported store.ts / useVpnState.ts consume it exactly as they consumed the
// mobile native module.
//
// When the Wails runtime is absent (plain `vite dev` preview or vitest) it
// falls back to the scripted MockOpenRungVpn, mirroring the mobile app's
// isMock pattern (openrung-mobile-app/src/native/OpenRungVpn.ts).
import { MockOpenRungVpn } from './mock';
import type { NativeVpnState, OpenRungVpnModule } from './types';
import type { RelayListResponse } from '../core/model/relay';

function wailsService() {
  return typeof window !== 'undefined' ? window.go?.vpnservice?.Service : undefined;
}

function wailsRuntime() {
  return typeof window !== 'undefined' ? window.runtime : undefined;
}

/** True when the scripted mock is in use instead of the real Go bridge. */
export const isMock = wailsService() == null || wailsRuntime() == null;

const mock: MockOpenRungVpn | null = isMock ? new MockOpenRungVpn() : null;

const realBridge: OpenRungVpnModule = {
  prepare: () => wailsService()!.Prepare(),
  // The Go binding takes plain strings; the contract's nulls map to "".
  connect: (brokerUrl, targetCountry, targetRelayId) =>
    wailsService()!.Connect(brokerUrl, targetCountry ?? '', targetRelayId ?? ''),
  disconnect: () => wailsService()!.Disconnect(),
  getState: () => wailsService()!.GetState(),
  getIdentity: () => wailsService()!.GetIdentity(),
  getProxyInfo: () => wailsService()!.GetProxyInfo(),
};

/** The active VPN module: the Go bridge when running under Wails, else the mock. */
export const OpenRungVpn: OpenRungVpnModule = mock ?? realBridge;

/**
 * Subscribes to openrungStateChanged (payload: NativeVpnState, emitted on every
 * status/log/relay/recents change). Returns an unsubscribe function.
 */
export function subscribeVpnState(callback: (state: NativeVpnState) => void): () => void {
  if (mock) {
    return mock.subscribe(callback);
  }
  return wailsRuntime()!.EventsOn('openrungStateChanged', callback);
}

/**
 * Fetches the relay list for the exit-node map directory. On desktop this runs
 * in Go (broker failover, 429 backoff, identity headers, no webview CORS); in
 * the mock it returns a sample list so the map renders offline.
 */
export function listRelaysForDirectory(): Promise<RelayListResponse> {
  if (mock) {
    return mock.listRelaysForDirectory();
  }
  return wailsService()!.ListRelaysForDirectory();
}

/** Copies text through Wails, with the browser clipboard as a preview fallback. */
export async function copyText(text: string): Promise<void> {
  const runtime = wailsRuntime();
  if (runtime != null) {
    await runtime.ClipboardSetText(text);
    return;
  }
  if (typeof navigator !== 'undefined' && navigator.clipboard != null) {
    await navigator.clipboard.writeText(text);
    return;
  }
  throw new Error('clipboard is unavailable');
}
