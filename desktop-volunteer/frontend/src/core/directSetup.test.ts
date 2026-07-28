import { describe, expect, it } from 'vitest';
import {
  AUTOMATIC_FALLBACK,
  LOCAL_SETUP_SCOPE,
  automaticPortSummary,
  directSetupActionFailure,
  directSetupTitle,
  relayHubSetupNote,
} from './directSetup';
import type { DirectSetupStatus } from '../native/types';

function status(overrides: Partial<DirectSetupStatus> = {}): DirectSetupStatus {
  return {
    platform: 'linux',
    state: 'needs_setup',
    reason: 'capability_missing',
    canEnable: true,
    canRemove: false,
    port: 443,
    message: 'The application does not currently have CAP_NET_BIND_SERVICE.',
    ...overrides,
  };
}

describe('direct setup messaging', () => {
  it('explains the narrow local scope without promising internet reachability', () => {
    expect(LOCAL_SETUP_SCOPE).toContain('only this computer');
    expect(LOCAL_SETUP_SCOPE).toContain('does not configure a router or NAT mapping');
    expect(LOCAL_SETUP_SCOPE).toContain('cloud firewall or security group');
    expect(LOCAL_SETUP_SCOPE).toContain('Automatic checks Internet reachability separately');
    expect(LOCAL_SETUP_SCOPE).toContain('Direct only does not use');
  });

  it('makes decline or elevation failure non-blocking', () => {
    const copy = directSetupActionFailure(new Error('authorization was cancelled'));
    expect(copy).toContain('authorization was cancelled');
    expect(copy).toContain('If you use Automatic');
    expect(copy).toContain(AUTOMATIC_FALLBACK);
  });

  it('shows why TCP 443 is unavailable while connected through RelayHub', () => {
    const copy = relayHubSetupNote(status());
    expect(copy).toContain('TCP 443 local setup needed');
    expect(copy).toContain('CAP_NET_BIND_SERVICE');
    expect(copy).toContain('alternate port');
    expect(copy).toContain('RelayHub');
  });

  it('does not guess why setup-ready candidates were not confirmed', () => {
    const copy = relayHubSetupNote(
      status({
        state: 'ready',
        reason: '',
        canEnable: false,
        canRemove: true,
        message: 'Ready.',
      }),
    );
    expect(copy).toContain('No direct candidate was positively confirmed');
    expect(copy).toContain('local bind problem');
    expect(copy).toContain('RelayHub probe API');
    expect(copy).toContain('Automatic is using RelayHub');
    expect(copy).toContain('console shows the candidate outcomes');
    expect(directSetupTitle(status({ state: 'ready' }))).toBe(
      'TCP 443 locally available',
    );
  });

  it('describes a deduplicated TCP 443 alternate as one candidate', () => {
    expect(automaticPortSummary(443)).toContain('TCP 443 only');
    expect(automaticPortSummary(443)).not.toContain('then alternate');
    expect(automaticPortSummary(8443)).toBe(
      'Automatic: TCP 443 first, then alternate direct port 8443',
    );
  });
});
