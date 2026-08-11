/**
 * Guards the wiring that makes the split-tunnel flush matter. store.test.ts
 * covers flushSplitTunnelPush in isolation, but the safety property lives at the
 * call site: native snapshots the routing preference when Connect starts, so a
 * connect that races the debounced push connects with the *previous* policy.
 * Without this file, dropping the `await` in useVpnState keeps the suite green.
 */
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { OpenRungVpn } from '../native/OpenRungVpn';
import { installMemoryLocalStorage } from '../test/memoryLocalStorage';
import { resetStoreForTests, setSplitTunnel } from './store';
import { useVpnState, type VpnStateHook } from './useVpnState';

installMemoryLocalStorage();
Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
  configurable: true,
  value: true,
});

let container: HTMLDivElement;
let root: Root;
let hook: VpnStateHook;

function Probe() {
  hook = useVpnState();
  return null;
}

describe('useVpnState connect ordering', () => {
  beforeEach(() => {
    localStorage.clear();
    resetStoreForTests();
    container = document.createElement('div');
    document.body.append(container);
    root = createRoot(container);
    vi.spyOn(OpenRungVpn, 'getState').mockResolvedValue({
      status: 'disconnected',
      relayLabel: null,
      lastError: null,
      logLines: [],
      recents: [],
    });
    vi.spyOn(OpenRungVpn, 'connect').mockResolvedValue();
    vi.spyOn(OpenRungVpn, 'prepare').mockResolvedValue(true);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    resetStoreForTests();
    vi.restoreAllMocks();
  });

  it('pushes the newest policy to native before dispatching connect', async () => {
    const order: string[] = [];
    vi.spyOn(OpenRungVpn, 'setSplitTunnelConfig').mockImplementation(async json => {
      order.push(`push:${json}`);
    });
    vi.mocked(OpenRungVpn.connect).mockImplementation(async () => {
      order.push('connect');
    });

    act(() => root.render(<Probe />));
    // The user turns split tunneling off and immediately hits Connect, well
    // inside the push debounce window.
    act(() => setSplitTunnel({ enabled: false }));
    await act(async () => {
      await hook.connect(null, null);
    });

    expect(order).toEqual([
      'push:{"version":1,"enabled":false,"bypass_lan":true,"bypass_countries":[],"excluded_packages":[]}',
      'connect',
    ]);
  });

  it('does not connect while the native push is still pending', async () => {
    let releasePush: (() => void) | undefined;
    vi.spyOn(OpenRungVpn, 'setSplitTunnelConfig').mockImplementation(
      () =>
        new Promise<void>(resolve => {
          releasePush = resolve;
        }),
    );

    act(() => root.render(<Probe />));
    act(() => setSplitTunnel({ bypassCountries: ['ir'] }));

    let connectSettled = false;
    const pending = hook.connect(null, null).then(() => {
      connectSettled = true;
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(OpenRungVpn.connect).not.toHaveBeenCalled();
    expect(connectSettled).toBe(false);

    releasePush?.();
    await act(async () => {
      await pending;
    });
    expect(OpenRungVpn.connect).toHaveBeenCalledTimes(1);
  });

  it('still connects when the native bridge rejects the push', async () => {
    vi.spyOn(OpenRungVpn, 'setSplitTunnelConfig').mockRejectedValue(new Error('bridge gone'));

    act(() => root.render(<Probe />));
    act(() => setSplitTunnel({ enabled: false }));
    await act(async () => {
      await hook.connect(null, null);
    });

    // A failed persist must not strand the user unable to connect at all.
    expect(OpenRungVpn.connect).toHaveBeenCalledTimes(1);
  });

  it('routes prepareAndConnect through the same flush', async () => {
    const order: string[] = [];
    vi.spyOn(OpenRungVpn, 'setSplitTunnelConfig').mockImplementation(async () => {
      order.push('push');
    });
    vi.mocked(OpenRungVpn.connect).mockImplementation(async () => {
      order.push('connect');
    });

    act(() => root.render(<Probe />));
    act(() => setSplitTunnel({ enabled: false }));
    await act(async () => {
      await hook.prepareAndConnect(null, null);
    });

    expect(order).toEqual(['push', 'connect']);
  });
});
