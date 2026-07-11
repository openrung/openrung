import { beforeEach, describe, expect, it } from 'vitest';
import { applyVolunteerState, getSnapshot, resetStoreForTests, subscribe } from './store';
import type { VolunteerState } from '../native/types';

function sampleState(overrides: Partial<VolunteerState> = {}): VolunteerState {
  return {
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
    consentAccepted: true,
    running: false,
    xrayFound: true,
    settings: {
      label: 'amber-otter-123',
      maxSessions: 8,
      maxMbps: 20,
      listenPort: 8443,
      brokerUrl: 'https://broker.openrung.org/',
      hubAddress: '',
      connectionMode: 'automatic',
    },
    ...overrides,
  };
}

beforeEach(() => {
  resetStoreForTests();
});

describe('store', () => {
  it('starts unhydrated with conservative defaults', () => {
    const snap = getSnapshot();
    expect(snap.hydrated).toBe(false);
    expect(snap.volunteer.phase).toBe('idle');
    expect(snap.volunteer.running).toBe(false);
    expect(snap.volunteer.consentAccepted).toBe(false);
  });

  it('applyVolunteerState mirrors the payload and marks hydration', () => {
    const next = sampleState({ phase: 'online', running: true, activeConnections: 3 });
    applyVolunteerState(next);
    const snap = getSnapshot();
    expect(snap.hydrated).toBe(true);
    expect(snap.volunteer).toBe(next);
  });

  it('returns a stable snapshot reference between updates', () => {
    applyVolunteerState(sampleState());
    expect(getSnapshot()).toBe(getSnapshot());
  });

  it('notifies subscribers on every apply and stops after unsubscribe', () => {
    let calls = 0;
    const unsubscribe = subscribe(() => {
      calls += 1;
    });

    applyVolunteerState(sampleState());
    expect(calls).toBe(1);

    applyVolunteerState(sampleState({ phase: 'starting', running: true }));
    expect(calls).toBe(2);

    unsubscribe();
    applyVolunteerState(sampleState({ phase: 'online', running: true }));
    expect(calls).toBe(2);
  });

  it('supports multiple independent subscribers', () => {
    let a = 0;
    let b = 0;
    const offA = subscribe(() => {
      a += 1;
    });
    subscribe(() => {
      b += 1;
    });

    applyVolunteerState(sampleState());
    offA();
    applyVolunteerState(sampleState({ phase: 'probing' }));

    expect(a).toBe(1);
    expect(b).toBe(2);
  });
});
