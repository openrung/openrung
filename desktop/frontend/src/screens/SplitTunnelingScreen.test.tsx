import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SplitTunnelingScreen } from './SplitTunnelingScreen';
import { SettingsScreen } from './SettingsScreen';
import { getSnapshot, resetStoreForTests, setSplitTunnel } from '../state/store';
import { installMemoryLocalStorage } from '../test/memoryLocalStorage';

installMemoryLocalStorage();
Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
  configurable: true,
  value: true,
});

let container: HTMLDivElement;
let root: Root;

function switches(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="switch"]'));
}

beforeEach(() => {
  localStorage.clear();
  resetStoreForTests();
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  if (root != null) {
    act(() => root.unmount());
  }
  container?.remove();
  resetStoreForTests();
});

describe('SplitTunnelingScreen', () => {
  it('renders proxy-only defaults and omits the mobile per-app picker', () => {
    act(() => root.render(<SplitTunnelingScreen onBack={() => {}} />));

    expect(container.textContent).toContain('BYPASS');
    expect(container.textContent).toContain('Iranian sites & services');
    expect(container.textContent).toContain('Chinese sites & services');
    expect(container.textContent).toContain('Apps that do not use the OpenRung proxy');
    expect(container.textContent).not.toContain('Bypassed apps');
    expect(switches()).toHaveLength(4);
    // Master + LAN on, both country presets off: a country preset leaks DNS for
    // a user who is not in that country, so it must be opted into explicitly.
    expect(switches().map(control => control.getAttribute('aria-checked'))).toEqual([
      'true',
      'true',
      'false',
      'false',
    ]);
    expect(document.activeElement?.textContent).toBe('Split tunneling');
  });

  it('disables every bypass preset when the master switch is off', () => {
    act(() => root.render(<SplitTunnelingScreen onBack={() => {}} />));

    act(() => switches()[0].click());

    const [master, ...presets] = switches();
    expect(master.getAttribute('aria-checked')).toBe('false');
    expect(presets.every(control => control.disabled)).toBe(true);
  });

  it('keeps country membership in the native ir,cn order', () => {
    setSplitTunnel({ bypassCountries: ['cn'] });
    act(() => root.render(<SplitTunnelingScreen onBack={() => {}} />));

    expect(switches()[2].getAttribute('aria-checked')).toBe('false');
    act(() => switches()[2].click());

    expect(getSnapshot().splitTunnel.bypassCountries).toEqual(['ir', 'cn']);
  });
});

describe('SettingsScreen split-tunnel entry', () => {
  it('reports the routing state and opens the sub-screen', async () => {
    const onOpen = vi.fn();
    await act(async () => {
      root.render(
        <SettingsScreen
          consoleOpen={false}
          connectionStatus="disconnected"
          onToggleConsole={() => {}}
          onOpenSplitTunneling={onOpen}
        />,
      );
      await Promise.resolve();
    });

    expect(container.textContent).toContain('On — selected proxy traffic bypasses the relay.');
    const row = Array.from(container.querySelectorAll<HTMLButtonElement>('button')).find(button =>
      button.textContent?.includes('Split tunneling'),
    );
    expect(row).toBeDefined();
    act(() => row?.click());
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
});
