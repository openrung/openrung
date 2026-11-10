// Checks the assumptions the mobile source makes about react-native-web, with
// the real library and the real mobile module. The mobile repository tests
// these paths against a stubbed `document`, because react-native-web is not
// installed there.
import { subscribeBackPress } from '@mobile/src/native/backPress';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { Modal, Text } from 'react-native-web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

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
