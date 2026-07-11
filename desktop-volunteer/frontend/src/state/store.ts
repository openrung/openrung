/**
 * Hand-rolled external store over the volunteer bridge state, following the
 * sibling desktop/frontend/src/state/store.ts pattern: module-level state, a
 * listener set, and useSyncExternalStore for React consumption. The Go service
 * owns all real state; this module only mirrors the latest VolunteerState
 * snapshot (plus a `hydrated` flag so the UI can hold off rendering the
 * consent gate until the first real snapshot arrives).
 */
import { useSyncExternalStore } from 'react';
import type { VolunteerState } from '../native/types';

export interface AppState {
  volunteer: VolunteerState; // mirrored from the Go bridge
  hydrated: boolean; // false until the first GetState()/event payload lands
}

const INITIAL_VOLUNTEER_STATE: VolunteerState = {
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
  consentAccepted: false,
  running: false,
  xrayFound: true,
  settings: {
    label: '',
    maxSessions: 8,
    maxMbps: 20,
    listenPort: 8443,
    brokerUrl: '',
    hubAddress: '',
    connectionMode: 'automatic',
  },
};

function initialState(): AppState {
  return { volunteer: INITIAL_VOLUNTEER_STATE, hydrated: false };
}

let state: AppState = initialState();
const listeners = new Set<() => void>();

function setState(next: AppState): void {
  state = next;
  for (const listener of listeners) {
    listener();
  }
}

export function getSnapshot(): AppState {
  return state;
}

export function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** React hook over the external store. */
export function useAppState(): AppState {
  return useSyncExternalStore(subscribe, getSnapshot);
}

/** Mirrors a VolunteerState (from GetState() or a volunteerStateChanged event) into the store. */
export function applyVolunteerState(volunteer: VolunteerState): void {
  setState({ volunteer, hydrated: true });
}

/** Test-only: resets the store to its initial state. */
export function resetStoreForTests(): void {
  state = initialState();
}
