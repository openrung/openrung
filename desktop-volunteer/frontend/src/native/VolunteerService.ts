// Desktop adapter over the Wails-bound Go volunteer Service. It maps the Go
// bindings (window.go.volunteerservice.Service.*, capitalized Go method names)
// and the volunteerStateChanged runtime event onto the VolunteerModule
// contract consumed by the store.
//
// When the Wails runtime is absent (plain `vite dev` preview or vitest) it
// falls back to the scripted MockVolunteerService, mirroring the sibling
// desktop app's isMock pattern (desktop/frontend/src/native/OpenRungVpn.ts).
import { MockVolunteerService } from './mock';
import type { VolunteerModule, VolunteerState } from './types';

function wailsService() {
  return typeof window !== 'undefined' ? window.go?.volunteerservice?.Service : undefined;
}

function wailsRuntime() {
  return typeof window !== 'undefined' ? window.runtime : undefined;
}

/** True when the scripted mock is in use instead of the real Go bridge. */
export const isMock = wailsService() == null || wailsRuntime() == null;

const mock: MockVolunteerService | null = isMock ? new MockVolunteerService() : null;

const realBridge: VolunteerModule = {
  getState: () => wailsService()!.GetState(),
  start: () => wailsService()!.Start(),
  stop: () => wailsService()!.Stop(),
  getSettings: () => wailsService()!.GetSettings(),
  saveSettings: settings => wailsService()!.SaveSettings(settings),
  regenerateLabel: () => wailsService()!.RegenerateLabel(),
  acceptConsent: () => wailsService()!.AcceptConsent(),
  running: () => wailsService()!.Running(),
};

/** The active volunteer module: the Go bridge under Wails, else the mock. */
export const VolunteerService: VolunteerModule = mock ?? realBridge;

/**
 * Subscribes to volunteerStateChanged (payload: VolunteerState, emitted on
 * every transition and about once per second while the relay is running).
 * Returns an unsubscribe function.
 */
export function subscribeVolunteerState(callback: (state: VolunteerState) => void): () => void {
  if (mock) {
    return mock.subscribe(callback);
  }
  return wailsRuntime()!.EventsOn('volunteerStateChanged', callback);
}
