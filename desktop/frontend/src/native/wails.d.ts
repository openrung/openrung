// Shape of the globals Wails injects into the webview at runtime. We call these
// directly instead of importing the generated frontend/wailsjs/* wrappers, so
// the Vite build and vitest do not depend on files that only exist after
// `wails dev`/`wails build`. When these globals are absent (a plain browser
// preview or a unit test) the adapter falls back to the scripted mock.
import type { NativeVpnState, NativeIdentity, NativeProxyInfo } from './types';
import type { RelayListResponse } from '../core/model/relay';

export interface WailsServiceBindings {
  Prepare(): Promise<boolean>;
  Connect(brokerUrl: string, targetCountry: string, targetRelayId: string): Promise<void>;
  Disconnect(): Promise<void>;
  GetState(): Promise<NativeVpnState>;
  GetIdentity(): Promise<NativeIdentity>;
  GetProxyInfo(): Promise<NativeProxyInfo>;
  ListRelaysForDirectory(): Promise<RelayListResponse>;
}

export interface WailsRuntime {
  EventsOn(event: string, callback: (data: NativeVpnState) => void): () => void;
  EventsOff(event: string): void;
  BrowserOpenURL(url: string): void;
  ClipboardSetText(text: string): Promise<boolean>;
}

declare global {
  interface Window {
    go?: {
      vpnservice?: {
        Service?: WailsServiceBindings;
      };
    };
    runtime?: WailsRuntime;
  }
}

export {};
