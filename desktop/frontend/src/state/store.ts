/**
 * PORTED from openrung-mobile-app/src/state/store.ts. Two desktop changes:
 *   1. AsyncStorage (RN native module) → localStorage (synchronous browser API).
 *   2. refreshDirectory's fetchRelays → the Go binding (listRelaysForDirectory),
 *      which owns broker candidate ordering / failover / 429 backoff. The
 *      injectable fetchRelays seam means loadExitNodeDirectory is reused verbatim.
 * Everything else — the external-store shape, supersession token, directory
 * status semantics — is unchanged from mobile.
 */
import { useSyncExternalStore } from 'react';
import { AppConfig } from '../core/config';
import type { DirectoryStatus, ExitNodeRegion, HomeViewMode } from '../core/model/exitNode';
import { loadExitNodeDirectory } from '../core/net/exitNodeDirectory';
import { listRelaysForDirectory } from '../native/OpenRungVpn';
import type { NativeVpnState } from '../native/types';

export interface AppState {
  native: NativeVpnState; // mirrored from the Go bridge
  brokerUrl: string; // fixed to config default (not editable)
  directoryStatus: DirectoryStatus;
  availableRegions: ExitNodeRegion[];
  languageTag: string; // '' = system, persisted in localStorage
  homeViewMode: HomeViewMode; // home directory presentation, persisted in localStorage
}

export const LANGUAGE_STORAGE_KEY = 'openrung.language';
export const HOME_VIEW_MODE_STORAGE_KEY = 'openrung.homeViewMode';

const INITIAL_NATIVE_STATE: NativeVpnState = {
  status: 'disconnected',
  relayLabel: null,
  lastError: null,
  logLines: [],
  recents: [],
};

function initialState(): AppState {
  return {
    native: INITIAL_NATIVE_STATE,
    brokerUrl: AppConfig.DEFAULT_BROKER_URL,
    directoryStatus: 'idle',
    availableRegions: [],
    languageTag: '',
    homeViewMode: 'map',
  };
}

// Best-effort synchronous persistence (localStorage can throw in private mode
// or when disabled); persistence is non-critical, so failures are swallowed.
function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Best-effort, like the mobile app's autoStoreLocales.
  }
}

let state: AppState = initialState();
const listeners = new Set<() => void>();

// Supersession token for directory loads: a newer (forced) refresh makes any
// in-flight load stale so its completion can't clobber state.
let directoryGeneration = 0;

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

/** Mirrors a NativeVpnState (from getState() or an openrungStateChanged event) into the store. */
export function applyNativeState(native: NativeVpnState): void {
  setState({ ...state, native });
}

/**
 * Refreshes the exit-node map directory. No-op while a load is in flight or
 * after a successful non-empty load, unless `force` is set (manual retry).
 * Returns a promise that settles when the load completes (never rejects).
 */
export function refreshDirectory(force: boolean = false): Promise<void> {
  const current = state;
  const alreadyLoaded =
    current.directoryStatus === 'loaded' && current.availableRegions.length > 0;
  if (!force && (current.directoryStatus === 'loading' || alreadyLoaded)) {
    return Promise.resolve();
  }

  const generation = ++directoryGeneration;
  setState({ ...state, directoryStatus: 'loading' });

  return loadExitNodeDirectory({ fetchRelays: () => listRelaysForDirectory() })
    .then(regions => {
      if (generation !== directoryGeneration) {
        return; // superseded by a newer refresh — don't clobber its result
      }
      setState({ ...state, availableRegions: regions, directoryStatus: 'loaded' });
    })
    .catch(() => {
      if (generation !== directoryGeneration) {
        return;
      }
      setState({ ...state, directoryStatus: 'failed' });
    });
}

/** Sets the in-app language tag ('' = system default) and persists it. */
export function setLanguageTag(tag: string): void {
  setState({ ...state, languageTag: tag });
  writeStored(LANGUAGE_STORAGE_KEY, tag);
}

/** Loads the persisted language selection (called once by the LanguageProvider on mount). */
export function hydrateLanguage(): void {
  const persisted = readStored(LANGUAGE_STORAGE_KEY);
  if (persisted !== null && persisted !== state.languageTag) {
    setState({ ...state, languageTag: persisted });
  }
}

/** Sets the home-screen directory presentation (map or list) and persists it. */
export function setHomeViewMode(mode: HomeViewMode): void {
  setState({ ...state, homeViewMode: mode });
  writeStored(HOME_VIEW_MODE_STORAGE_KEY, mode);
}

/** Loads the persisted home view mode. */
export function hydrateHomeViewMode(): void {
  const persisted = readStored(HOME_VIEW_MODE_STORAGE_KEY);
  if ((persisted === 'map' || persisted === 'list') && persisted !== state.homeViewMode) {
    setState({ ...state, homeViewMode: persisted });
  }
}

/** Test-only: resets the store to its initial state. */
export function resetStoreForTests(): void {
  directoryGeneration++;
  state = initialState();
}
