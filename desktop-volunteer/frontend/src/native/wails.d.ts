// Shape of the globals Wails injects into the webview at runtime. We call these
// directly instead of importing the generated frontend/wailsjs/* wrappers, so
// the Vite build and vitest do not depend on files that only exist after
// `wails dev`/`wails build`. When these globals are absent (a plain browser
// preview or a unit test) the adapter falls back to the scripted mock.
import type { VolunteerSettings, VolunteerState } from './types';

export interface WailsServiceBindings {
  GetState(): Promise<VolunteerState>;
  Start(): Promise<void>;
  Stop(): Promise<void>;
  GetSettings(): Promise<VolunteerSettings>;
  SaveSettings(settings: VolunteerSettings): Promise<VolunteerSettings>;
  RegenerateLabel(): Promise<string>;
  AcceptConsent(): Promise<void>;
  Running(): Promise<boolean>;
}

export interface WailsRuntime {
  EventsOn(event: string, callback: (data: VolunteerState) => void): () => void;
  EventsOff(event: string): void;
  BrowserOpenURL(url: string): void;
}

declare global {
  interface Window {
    go?: {
      volunteerservice?: {
        Service?: WailsServiceBindings;
      };
    };
    runtime?: WailsRuntime;
  }
}

export {};
