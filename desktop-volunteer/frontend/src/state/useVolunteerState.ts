/**
 * The one place the app touches the bridge lifecycle: seeds the store with
 * GetState() on mount and subscribes to volunteerStateChanged exactly once.
 * Only App calls this hook; children read the store via props so the event
 * subscription is never duplicated.
 */
import { useCallback, useEffect } from 'react';
import { VolunteerService, subscribeVolunteerState } from '../native/VolunteerService';
import { applyVolunteerState, useAppState } from './store';
import type { AppState } from './store';

export interface VolunteerStateHook {
  state: AppState;
  /** starting | probing | registering | stopping */
  isTransitioning: boolean;
  /** phase === 'online' */
  isOnline: boolean;
  start: () => Promise<void>;
  stop: () => Promise<void>;
  acceptConsent: () => Promise<void>;
}

export function useVolunteerState(): VolunteerStateHook {
  const state = useAppState();

  useEffect(() => {
    let mounted = true;
    VolunteerService.getState()
      .then(volunteerState => {
        if (mounted) {
          applyVolunteerState(volunteerState);
        }
      })
      .catch(() => {
        // Bridge state stays at the store default until the first event arrives.
      });
    const unsubscribe = subscribeVolunteerState(applyVolunteerState);
    return () => {
      mounted = false;
      unsubscribe();
    };
  }, []);

  const start = useCallback(() => VolunteerService.start(), []);
  const stop = useCallback(() => VolunteerService.stop(), []);
  const acceptConsent = useCallback(() => VolunteerService.acceptConsent(), []);

  const phase = state.volunteer.phase;
  const isTransitioning =
    phase === 'starting' || phase === 'probing' || phase === 'registering' || phase === 'stopping';
  const isOnline = phase === 'online';

  return { state, isTransitioning, isOnline, start, stop, acceptConsent };
}
