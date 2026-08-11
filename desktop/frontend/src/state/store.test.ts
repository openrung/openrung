import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { OpenRungVpn } from '../native/OpenRungVpn';
import { installMemoryLocalStorage } from '../test/memoryLocalStorage';
import {
  flushSplitTunnelPush,
  getSnapshot,
  hydrateSplitTunnel,
  resetStoreForTests,
  setSplitTunnel,
  SPLIT_TUNNEL_STORAGE_KEY,
} from './store';

const defaults = {
  enabled: true,
  bypassLan: true,
  bypassCountries: ['ir', 'cn'],
  excludedApps: [],
};

installMemoryLocalStorage();

describe('split-tunnel store', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    resetStoreForTests();
    vi.spyOn(OpenRungVpn, 'setSplitTunnelConfig').mockResolvedValue();
  });

  afterEach(() => {
    resetStoreForTests();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('persists and pushes the enabled mobile defaults on a fresh install', async () => {
    hydrateSplitTunnel();

    expect(getSnapshot().splitTunnel).toEqual(defaults);
    expect(localStorage.getItem(SPLIT_TUNNEL_STORAGE_KEY)).toBe(JSON.stringify(defaults));
    expect(OpenRungVpn.setSplitTunnelConfig).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1200);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledWith(
      '{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["ir","cn"],"excluded_packages":[]}',
    );
  });

  it('collapses rapid edits into one native push of the final state', async () => {
    setSplitTunnel({ enabled: false });
    await vi.advanceTimersByTimeAsync(600);
    setSplitTunnel({ enabled: true, bypassLan: false, bypassCountries: ['cn'] });

    await vi.advanceTimersByTimeAsync(1199);
    expect(OpenRungVpn.setSplitTunnelConfig).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);

    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledTimes(1);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledWith(
      '{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["cn"],"excluded_packages":[]}',
    );
    expect(JSON.parse(localStorage.getItem(SPLIT_TUNNEL_STORAGE_KEY) ?? '')).toEqual({
      ...defaults,
      bypassLan: false,
      bypassCountries: ['cn'],
    });
  });

  it('hydrates valid state, filters unknown countries, and keeps app exclusions empty', async () => {
    localStorage.setItem(
      SPLIT_TUNNEL_STORAGE_KEY,
      JSON.stringify({
        enabled: false,
        bypassLan: false,
        bypassCountries: ['us', 'ir', 7],
        excludedApps: ['legacy.desktop.selection', 4],
      }),
    );

    hydrateSplitTunnel();
    expect(getSnapshot().splitTunnel).toEqual({
      enabled: false,
      bypassLan: false,
      bypassCountries: ['ir'],
      excludedApps: [],
    });

    await vi.advanceTimersByTimeAsync(1200);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledWith(
      '{"version":1,"enabled":false,"bypass_lan":false,"bypass_countries":["ir"],"excluded_packages":[]}',
    );
  });

  it('does not overwrite native when browser persistence is malformed', async () => {
    localStorage.setItem(SPLIT_TUNNEL_STORAGE_KEY, 'not json');

    hydrateSplitTunnel();
    await vi.advanceTimersByTimeAsync(1200);

    expect(getSnapshot().splitTunnel).toEqual(defaults);
    expect(OpenRungVpn.setSplitTunnelConfig).not.toHaveBeenCalled();
  });

  it('flushes fresh defaults before the first native connect snapshot', async () => {
    await flushSplitTunnelPush();

    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledTimes(1);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledWith(
      '{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["ir","cn"],"excluded_packages":[]}',
    );

    await vi.advanceTimersByTimeAsync(1200);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledTimes(1);
  });

  it('waits for an already-started native push before connect can continue', async () => {
    let finishPush: (() => void) | undefined;
    vi.mocked(OpenRungVpn.setSplitTunnelConfig).mockReturnValue(
      new Promise<void>(resolve => {
        finishPush = resolve;
      }),
    );
    hydrateSplitTunnel();
    await vi.advanceTimersByTimeAsync(1200);

    let flushed = false;
    const flush = flushSplitTunnelPush().then(() => {
      flushed = true;
    });
    await Promise.resolve();
    expect(flushed).toBe(false);

    finishPush?.();
    await flush;
    expect(flushed).toBe(true);
  });

  it('flushes a newer edit made while an older native push is in flight', async () => {
    let finishFirstPush: (() => void) | undefined;
    vi.mocked(OpenRungVpn.setSplitTunnelConfig)
      .mockReturnValueOnce(
        new Promise<void>(resolve => {
          finishFirstPush = resolve;
        }),
      )
      .mockResolvedValue();
    hydrateSplitTunnel();
    await vi.advanceTimersByTimeAsync(1200);

    const flush = flushSplitTunnelPush();
    setSplitTunnel({ bypassLan: false, bypassCountries: ['cn'] });
    finishFirstPush?.();
    await flush;

    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledTimes(2);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenLastCalledWith(
      '{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["cn"],"excluded_packages":[]}',
    );
    await vi.advanceTimersByTimeAsync(1200);
    expect(OpenRungVpn.setSplitTunnelConfig).toHaveBeenCalledTimes(2);
  });

  it('bounds a wedged native push so connect can fail toward the proxy', async () => {
    vi.mocked(OpenRungVpn.setSplitTunnelConfig).mockReturnValue(new Promise<void>(() => {}));

    let flushed = false;
    const flush = flushSplitTunnelPush().then(() => {
      flushed = true;
    });
    await vi.advanceTimersByTimeAsync(2999);
    expect(flushed).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await flush;
    expect(flushed).toBe(true);
  });
});
