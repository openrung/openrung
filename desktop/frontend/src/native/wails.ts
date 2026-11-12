// Shape of the globals Wails injects into the webview at runtime (the Go
// service bound in desktop/main.go). Absent in a plain browser preview. The
// payloads are passed through to the app untouched, so only the fields this
// adapter itself reads are typed.

export interface WailsRelayListResponse {
  count: number;
  server_time: string;
  relays: unknown[];
  key_id?: string;
}

/** vpnservice.NativeProxyInfo, as the app's OpenRungDesktop contract consumes it. */
export interface WailsProxyInfo {
  endpoint: string;
  persistenceWarning: string | null;
  shellIntegration: boolean;
  shellIntegrationError: string | null;
  enableCommand: string;
  disableCommand: string;
}

/** vpnservice.NativeTunnelMode, as the app's OpenRungDesktop contract consumes it. */
export interface WailsTunnelMode {
  mode: 'proxy' | 'tun';
  available: boolean;
  requiresRestart: boolean;
}

interface WailsServiceBindings {
  Prepare(): Promise<boolean>;
  Connect(brokerUrl: string, targetCountry: string, targetRelayId: string): Promise<void>;
  Disconnect(): Promise<void>;
  GetState(): Promise<unknown>;
  GetIdentity(): Promise<{ clientId: string; sessionId: string | null }>;
  SetSplitTunnelConfig(configJson: string): Promise<void>;
  GetProxyInfo(): Promise<WailsProxyInfo>;
  GetTunnelMode(): Promise<WailsTunnelMode>;
  SetTunnelMode(mode: string): Promise<void>;
  ListRelaysForDirectory(): Promise<WailsRelayListResponse>;
  FetchManifestCandidate(candidateUrl: string): Promise<{ sourceUrl: string; bodyJson: string }>;
}

interface WailsRuntime {
  EventsOn(event: string, callback: (data: unknown) => void): () => void;
  BrowserOpenURL(url: string): void;
  ClipboardSetText(text: string): Promise<boolean>;
}

declare global {
  /** desktop/VERSION, injected by vite.config.ts. */
  const __APP_VERSION__: string;

  interface Window {
    go?: { vpnservice?: { Service?: WailsServiceBindings } };
    runtime?: WailsRuntime;
  }
}
