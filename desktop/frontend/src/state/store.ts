/**
 * PORTED from openrung-mobile-app/src/state/store.ts. Two desktop changes:
 *   1. AsyncStorage (RN native module) → localStorage (synchronous browser API).
 *   2. refreshDirectory's fetchRelays → the Go binding (listRelaysForDirectory),
 *      which owns broker candidate ordering / failover / 429 backoff. The
 *      injectable fetchRelays seam means loadExitNodeDirectory is reused verbatim.
 *   3. Split-tunnel preferences use synchronous localStorage while the native
 *      bridge remains asynchronous.
 * Directory supersession and status semantics remain aligned with mobile.
 */
import { useSyncExternalStore } from 'react';
import { AppConfig } from '../core/config';
import type { DirectoryStatus, ExitNodeRegion, HomeViewMode } from '../core/model/exitNode';
import { loadExitNodeDirectory } from '../core/net/exitNodeDirectory';
import { listRelaysForDirectory, OpenRungVpn } from '../native/OpenRungVpn';
import type { NativeVpnState } from '../native/types';

export interface SplitTunnelState {
  enabled: boolean;
  bypassLan: boolean;
  bypassCountries: string[]; // lowercase ISO codes; v1 recognizes only ir/cn
  excludedApps: string[]; // always empty on proxy-only desktop; kept for contract parity
}

export interface AppState {
  native: NativeVpnState; // mirrored from the Go bridge
  brokerUrl: string; // fixed to config default (not editable)
  directoryStatus: DirectoryStatus;
  availableRegions: ExitNodeRegion[];
  languageTag: string; // '' = system, persisted in localStorage
  homeViewMode: HomeViewMode; // home directory presentation, persisted in localStorage
  splitTunnel: SplitTunnelState; // persisted locally and mirrored to the Go service
}

export const LANGUAGE_STORAGE_KEY = 'openrung.language';
export const HOME_VIEW_MODE_STORAGE_KEY = 'openrung.homeViewMode';
export const SPLIT_TUNNEL_STORAGE_KEY = 'openrung.splitTunnel';

const INITIAL_NATIVE_STATE: NativeVpnState = {
  status: 'disconnected',
  relayLabel: null,
  lastError: null,
  logLines: [],
  recents: [],
};

// LAN bypass on, country presets OFF. A country preset routes that country's
// geosite domains to an in-country resolver over cleartext UDP outside the
// tunnel, so enabling one for a country the user is not in hands their DNS to a
// foreign state resolver and connects direct from their real IP. The presets are
// opt-in for that reason; LAN-only reproduces the pre-split-tunnel behavior.
const INITIAL_SPLIT_TUNNEL: SplitTunnelState = {
  enabled: true,
  bypassLan: true,
  bypassCountries: [],
  excludedApps: [],
};

function initialState(): AppState {
  return {
    native: INITIAL_NATIVE_STATE,
    brokerUrl: AppConfig.DEFAULT_BROKER_URL,
    directoryStatus: 'idle',
    availableRegions: [],
    languageTag: '',
    homeViewMode: 'map',
    splitTunnel: INITIAL_SPLIT_TUNNEL,
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

const SPLIT_TUNNEL_PUSH_DEBOUNCE_MS = 1200;
const SPLIT_TUNNEL_PUSH_TIMEOUT_MS = 3000;
let splitTunnelPushTimer: ReturnType<typeof setTimeout> | null = null;
let splitTunnelHydrated = false;
let splitTunnelPushEpoch = 0;
let splitTunnelRevision = 0;
let splitTunnelPushChain: Promise<void> = Promise.resolve();

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

/** Serializes the shared v1 bridge contract in the stable native comparison order. */
function splitTunnelConfigJson(split: SplitTunnelState): string {
  return JSON.stringify({
    version: 1,
    enabled: split.enabled,
    bypass_lan: split.bypassLan,
    bypass_countries: split.bypassCountries,
    excluded_packages: split.excludedApps,
  });
}

/** Queues bridge writes in order so an older slow call cannot overwrite a
 * newer preference. A bridge failure must never block Settings or Connect. */
function pushSplitTunnelToNative(): Promise<void> {
  const configJson = splitTunnelConfigJson(state.splitTunnel);
  const epoch = splitTunnelPushEpoch;
  const push = async () => {
    if (epoch !== splitTunnelPushEpoch) {
      return;
    }
    let bridgePush: Promise<void>;
    try {
      bridgePush = Promise.resolve(OpenRungVpn.setSplitTunnelConfig(configJson));
    } catch {
      // Supports a newer frontend running against a stale desktop binary.
      return;
    }
    // Native persistence should be fast, but a stale or wedged bridge must not
    // make Connect unavailable forever. The native service serializes calls,
    // so letting the UI proceed after this bound still preserves Go-side order
    // if the original invocation eventually completes.
    await new Promise<void>(resolve => {
      const timeout = setTimeout(resolve, SPLIT_TUNNEL_PUSH_TIMEOUT_MS);
      bridgePush.then(
        () => {
          clearTimeout(timeout);
          resolve();
        },
        () => {
          clearTimeout(timeout);
          resolve();
        },
      );
    });
  };
  splitTunnelPushChain = splitTunnelPushChain.then(push, push);
  return splitTunnelPushChain;
}

function scheduleSplitTunnelPush(): void {
  if (splitTunnelPushTimer != null) {
    clearTimeout(splitTunnelPushTimer);
  }
  splitTunnelPushTimer = setTimeout(() => {
    splitTunnelPushTimer = null;
    void pushSplitTunnelToNative();
  }, SPLIT_TUNNEL_PUSH_DEBOUNCE_MS);
}

/** Validates browser-persisted state and drops unknown v1 country presets. */
function parsePersistedSplitTunnel(raw: string): SplitTunnelState | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed == null) {
    return null;
  }
  const candidate = parsed as Record<string, unknown>;
  const { enabled, bypassLan, bypassCountries, excludedApps } = candidate;
  if (
    typeof enabled !== 'boolean' ||
    typeof bypassLan !== 'boolean' ||
    !Array.isArray(bypassCountries) ||
    !Array.isArray(excludedApps)
  ) {
    return null;
  }
  const isString = (value: unknown): value is string => typeof value === 'string';
  return {
    enabled,
    bypassLan,
    bypassCountries: bypassCountries
      .filter(isString)
      .filter(code => code === 'ir' || code === 'cn'),
    // Proxy enrollment is already per-app on desktop, so package exclusions
    // never become part of the effective desktop policy.
    excludedApps: [],
  };
}

/**
 * Loads split-tunnel preferences once and mirrors them to native. Fresh
 * installs materialize the mobile defaults (enabled, LAN + Iran + China).
 * Malformed storage does not overwrite a potentially valid native config.
 */
export function hydrateSplitTunnel(): void {
  if (splitTunnelHydrated) {
    return;
  }

  let persisted: string | null;
  try {
    persisted = localStorage.getItem(SPLIT_TUNNEL_STORAGE_KEY);
  } catch {
    return;
  }
  splitTunnelHydrated = true;

  if (persisted == null) {
    writeStored(SPLIT_TUNNEL_STORAGE_KEY, JSON.stringify(state.splitTunnel));
    scheduleSplitTunnelPush();
    return;
  }

  const parsed = parsePersistedSplitTunnel(persisted);
  if (parsed == null) {
    return;
  }
  if (JSON.stringify(parsed) !== JSON.stringify(state.splitTunnel)) {
    splitTunnelRevision++;
    setState({ ...state, splitTunnel: parsed });
  }
  scheduleSplitTunnelPush();
}

/** Persists a local edit and debounces an active-connection reapply. */
export function setSplitTunnel(patch: Partial<SplitTunnelState>): void {
  splitTunnelHydrated = true;
  const splitTunnel = { ...state.splitTunnel, ...patch, excludedApps: [] };
  splitTunnelRevision++;
  setState({ ...state, splitTunnel });
  writeStored(SPLIT_TUNNEL_STORAGE_KEY, JSON.stringify(splitTunnel));
  scheduleSplitTunnelPush();
}

/** Flushes initialization or a pending edit before native snapshots a connect. */
export async function flushSplitTunnelPush(): Promise<void> {
  hydrateSplitTunnel();
  // Re-check after every await. A user can edit the policy while an older
  // bridge push is resolving; Connect must snapshot the newest revision, not
  // merely whichever call happened to be in flight when the button was hit.
  for (;;) {
    const revision = splitTunnelRevision;
    if (splitTunnelPushTimer != null) {
      clearTimeout(splitTunnelPushTimer);
      splitTunnelPushTimer = null;
      void pushSplitTunnelToNative();
    }
    const chain = splitTunnelPushChain;
    await chain;
    if (
      revision === splitTunnelRevision &&
      splitTunnelPushTimer == null &&
      chain === splitTunnelPushChain
    ) {
      return;
    }
  }
}

/** Test-only: resets the store to its initial state. */
export function resetStoreForTests(): void {
  directoryGeneration++;
  splitTunnelHydrated = false;
  splitTunnelPushEpoch++;
  splitTunnelRevision = 0;
  splitTunnelPushChain = Promise.resolve();
  if (splitTunnelPushTimer != null) {
    clearTimeout(splitTunnelPushTimer);
    splitTunnelPushTimer = null;
  }
  state = initialState();
}
