// Minimal typings for this package's own files. The mobile source is
// type-checked in its own repository (against react-native's types), not here.

declare module '@mobile/App' {
  import type { ComponentType } from 'react';

  const App: ComponentType;
  export default App;
}

declare module '@mobile/src/native/backPress' {
  export function subscribeBackPress(handler: () => boolean): () => void;
}

declare module 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url' {
  const url: string;
  export default url;
}

declare module 'react-native-web' {
  import type { ComponentType, ReactNode } from 'react';

  export const AppRegistry: {
    registerComponent(name: string, getComponent: () => ComponentType): void;
    runApplication(name: string, parameters: { rootTag: HTMLElement | null }): void;
  };
  export const NativeModules: Record<string, unknown>;
  export const DeviceEventEmitter: {
    emit(eventType: string, ...args: unknown[]): void;
    addListener(eventType: string, listener: (...args: unknown[]) => void): { remove(): void };
  };
  export const Linking: { openURL(url: string): Promise<void> };
  export const StyleSheet: { create<T extends Record<string, object>>(styles: T): T };
  export const View: ComponentType<{
    style?: unknown;
    accessibilityLabel?: string;
    children?: ReactNode;
  }>;
  export const Text: ComponentType<{ children?: ReactNode }>;
  export const Modal: ComponentType<{
    visible?: boolean;
    onRequestClose?: () => void;
    children?: ReactNode;
  }>;
}

declare module '@mobile/src/state/store' {
  export function getSnapshot(): {
    update: { tier: 'none' | 'available' | 'notify' | 'blocked'; latestVersion: string | null };
  };
}

declare module '@mobile/src/state/updateCheck' {
  /** Starts the app's update check; returns its cleanup. */
  export function startUpdateCheck(): () => void;
}
