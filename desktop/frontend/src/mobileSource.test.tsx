// Checks the assumptions the mobile source makes about react-native-web, with
// the real library and the real mobile module. The mobile repository tests
// these paths against a stubbed `document`, because react-native-web is not
// installed there.
import { subscribeBackPress } from '@mobile/src/native/backPress';
import { getSnapshot } from '@mobile/src/state/store';
import { startUpdateCheck } from '@mobile/src/state/updateCheck';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { Modal, NativeModules, Text } from 'react-native-web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { installNativeModules } from './native/installNativeModules';
import { Map as MapView } from './shims/maplibre';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLElement;
let root: Root;

beforeEach(() => {
  host = document.createElement('div');
  document.body.appendChild(host);
  root = createRoot(host);
});

afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

function pressEscape(): void {
  for (const type of ['keydown', 'keyup']) {
    document.dispatchEvent(new KeyboardEvent(type, { key: 'Escape', bubbles: true }));
  }
}

describe('Escape as the back gesture', () => {
  it('goes back when no modal is open', () => {
    const goBack = vi.fn(() => true);
    const unsubscribe = subscribeBackPress(goBack);
    pressEscape();
    unsubscribe();
    expect(goBack).toHaveBeenCalledTimes(1);
  });

  it('closes an open Modal instead of navigating behind it', () => {
    const goBack = vi.fn(() => true);
    const onRequestClose = vi.fn();
    const unsubscribe = subscribeBackPress(goBack);
    act(() =>
      root.render(
        createElement(Modal, { visible: true, onRequestClose }, createElement(Text, null, 'picker')),
      ),
    );

    pressEscape();
    unsubscribe();
    expect(onRequestClose).toHaveBeenCalledTimes(1);
    expect(goBack).not.toHaveBeenCalled();
  });
});

describe('the map without WebGL', () => {
  it('renders an empty map instead of taking the app down', () => {
    // A webview whose GPU path is unavailable hands out no WebGL context.
    const getContext = vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const mapStyle = { version: 8 as const, sources: {}, layers: [] };
    expect(() =>
      act(() => root.render(createElement(MapView, { mapStyle }, createElement(Text, null, 'child')))),
    ).not.toThrow();
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
    getContext.mockRestore();
  });
});

describe('the update check in the desktop client', () => {
  it('reads the desktop section of a manifest the Go service fetched', async () => {
    // Unsigned, so the app offers the passive Settings row and nothing stronger.
    const payload = {
      schema: 1,
      generated_at: '2026-11-01T00:00:00Z',
      android: { latest: '0.0.1', min_supported: '0.0.0' },
      ios: { latest: '0.0.1', min_supported: '0.0.0' },
      desktop: { latest: '999.0.0', min_supported: '0.0.0' },
      promote: 'silent',
      notice: null,
    };
    const envelope = JSON.stringify({ schema: 1, payload_b64: btoa(JSON.stringify(payload)) });
    const FetchManifestCandidate = vi.fn(async (candidateUrl: string) => ({
      sourceUrl: candidateUrl,
      bodyJson: envelope,
    }));
    window.go = { vpnservice: { Service: { FetchManifestCandidate } as never } };
    window.runtime = { EventsOn: () => () => undefined } as never;
    installNativeModules();

    const stop = startUpdateCheck();
    try {
      await vi.waitFor(() => expect(getSnapshot().update.tier).toBe('available'));
      expect(getSnapshot().update.latestVersion).toBe('999.0.0');
      // Every candidate goes through the host, the GitHub asset included.
      expect(FetchManifestCandidate.mock.calls.map(([url]) => url)).toContain(
        'https://github.com/openrung/openrung-mobile-app/releases/latest/download/update-manifest.json',
      );
    } finally {
      stop();
      delete (NativeModules as Record<string, unknown>).OpenRungVpn;
      delete (NativeModules as Record<string, unknown>).OpenRungBroker;
      delete (NativeModules as Record<string, unknown>).OpenRungDesktop;
      delete window.go;
      delete window.runtime;
    }
  });
});
