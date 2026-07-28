import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VolunteerService } from '../native/VolunteerService';
import type { DirectSetupStatus, VolunteerState } from '../native/types';
import { HomeScreen } from './HomeScreen';
import { SettingsScreen } from './SettingsScreen';

(
  globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
).IS_REACT_ACT_ENVIRONMENT = true;

afterEach(() => vi.restoreAllMocks());

function setupStatus(
  overrides: Partial<DirectSetupStatus> = {},
): DirectSetupStatus {
  return {
    platform: 'linux',
    state: 'needs_setup',
    reason: 'capability_missing',
    canEnable: true,
    canRemove: false,
    port: 443,
    message: 'CAP_NET_BIND_SERVICE is missing from this application binary.',
    ...overrides,
  };
}

function volunteerState(overrides: Partial<VolunteerState> = {}): VolunteerState {
  return {
    phase: 'idle',
    transport: '',
    relayLabel: 'quiet-pine-123',
    relayId: '',
    publicEndpoint: '',
    lastError: null,
    startedAtMs: 0,
    activeConnections: 0,
    totalConnections: 0,
    bytesFromClients: 0,
    bytesToClients: 0,
    logLines: [],
    consentAccepted: true,
    running: false,
    xrayFound: true,
    directSetup: setupStatus(),
    settings: {
      label: 'quiet-pine-123',
      maxSessions: 75,
      maxMbps: 100,
      listenPort: 8443,
      brokerUrl: 'https://broker.openrung.org/',
      hubAddress: 'relayhub.example:9443',
      connectionMode: 'automatic',
    },
    ...overrides,
  };
}

describe('direct setup UI', () => {
  it('offers setup explicitly and explains non-blocking, local-only scope', () => {
    const html = renderToStaticMarkup(
      <SettingsScreen state={volunteerState()} />,
    );

    expect(html).toContain('Enable direct connections');
    expect(html).toContain('does not configure a router or NAT mapping');
    expect(html).toContain('cloud firewall or security group');
    expect(html).toContain('If you use Automatic, volunteering remains available');
    expect(html).toContain('Automatic: TCP 443 first, then alternate direct port 8443');
  });

  it('shows removal only when the managed setup is reversible', () => {
    const html = renderToStaticMarkup(
      <SettingsScreen
        state={volunteerState({
          directSetup: setupStatus({
            state: 'ready',
            reason: '',
            canEnable: false,
            canRemove: true,
            message: 'Local setup is ready.',
          }),
        })}
      />,
    );

    expect(html).toContain('TCP 443 locally available');
    expect(html).toContain('Remove local setup');
    expect(html).not.toContain('Enable direct connections');
  });

  it('does not imply that Direct only will use Automatic fallback', async () => {
    const current = volunteerState();
    const container = document.createElement('div');
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <SettingsScreen
          state={{
            ...current,
            settings: { ...current.settings, connectionMode: 'direct' },
          }}
        />,
      );
      await Promise.resolve();
    });
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.vol-accordion-toggle')!.click();
    });

    expect(container.textContent).toContain('Direct only: TCP 8443');
    expect(container.textContent).toContain('If you use Automatic, volunteering remains available');
    expect(container.textContent).toContain(
      'never falls back to RelayHub or performs its reachability check',
    );
    expect(container.textContent).toContain('publicly reachable address');

    await act(async () => root.unmount());
  });

  it('describes a configured 443 alternate as one deduplicated candidate', async () => {
    const current = volunteerState();
    const container = document.createElement('div');
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <SettingsScreen
          state={{
            ...current,
            settings: { ...current.settings, listenPort: 443 },
          }}
        />,
      );
      await Promise.resolve();
    });
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.vol-accordion-toggle')!.click();
    });

    expect(container.textContent).toContain('Automatic: TCP 443 only');
    expect(container.textContent).toContain('checks TCP 443 once');
    expect(container.textContent).not.toContain('then alternate direct port 443');
    expect(container.textContent).not.toContain('then the alternate port below');

    await act(async () => root.unmount());
  });

  it('locks setup changes while an advertised session is running', () => {
    const html = renderToStaticMarkup(
      <SettingsScreen state={volunteerState({ running: true })} />,
    );

    expect(html).toContain('Stop volunteering to change settings.');
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Enable direct connections<\/button>/);
  });

  it('shows the Linux post-removal restart gate without another setup action', () => {
    const html = renderToStaticMarkup(
      <SettingsScreen
        state={volunteerState({
          directSetup: setupStatus({
            state: 'unavailable',
            reason: 'removal_restart_required',
            canEnable: false,
            canRemove: false,
            message:
              'The file capability was removed. Quit and reopen before volunteering again.',
          }),
        })}
      />,
    );

    expect(html).toContain('Quit and reopen before volunteering again');
    expect(html).not.toContain('Enable direct connections');
    expect(html).not.toContain('Remove local setup');
  });

  it('shows RelayHub without guessing which candidate failure occurred', () => {
    const html = renderToStaticMarkup(
      <HomeScreen
        state={volunteerState({
          phase: 'online',
          transport: 'tunnel',
          running: true,
          directSetup: setupStatus({
            state: 'ready',
            reason: '',
            canEnable: false,
            canRemove: true,
            message: 'Local setup is ready.',
          }),
        })}
        onStart={async () => undefined}
        onStop={async () => undefined}
      />,
    );

    expect(html).toContain('Via RelayHub');
    expect(html).toContain('No direct candidate was positively confirmed');
    expect(html).toContain('local bind problem');
    expect(html).toContain('RelayHub probe API');
    expect(html).toContain('console shows the candidate outcomes');
  });

  it('refreshes setup status read-only on mount and never enables automatically', async () => {
    const refreshed = setupStatus({
      state: 'ready',
      reason: 'ready',
      canEnable: false,
      canRemove: true,
      message: 'Refreshed local setup is ready.',
    });
    const getStatus = vi
      .spyOn(VolunteerService, 'getDirectSetupStatus')
      .mockResolvedValue(refreshed);
    const enable = vi
      .spyOn(VolunteerService, 'enableDirectConnections')
      .mockResolvedValue(refreshed);
    const container = document.createElement('div');
    const root = createRoot(container);

    await act(async () => {
      root.render(<SettingsScreen state={volunteerState()} />);
      await Promise.resolve();
    });

    expect(getStatus).toHaveBeenCalledTimes(1);
    expect(enable).not.toHaveBeenCalled();
    expect(container.textContent).toContain('TCP 443 locally available');
    expect(container.textContent).toContain('Refreshed local setup is ready.');

    await act(async () => root.unmount());
  });
});
