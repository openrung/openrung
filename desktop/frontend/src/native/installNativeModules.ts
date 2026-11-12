// The desktop "native side" of the mobile app's bridge contract. The mobile
// source talks to NativeModules.OpenRungVpn / OpenRungBroker / OpenRungDesktop
// and listens for the openrungStateChanged device event; here those are backed
// by the Wails-bound Go service (window.go.vpnservice.Service), so the mobile
// state layer runs unchanged.
//
// Without the Wails runtime (a plain `vite dev` browser preview) OpenRungVpn is
// left unset, which makes the mobile app use its own scripted mock, and the
// directory and proxy details are served from fixtures so every screen renders.
import { DeviceEventEmitter, Linking, NativeModules } from 'react-native-web';

import { previewProxyInfo, previewRelayList, previewTunnelMode } from './previewFixtures';

const STATE_EVENT = 'openrungStateChanged';

function unavailable(what: string): Error {
  return Object.assign(new Error(`${what} is not available in the desktop client`), {
    code: 'unavailable',
  });
}

/** NativeModules.OpenRungBroker, reduced to the relay directory fetch. */
function brokerModule(listRelays: () => Promise<{ key_id?: string }>) {
  return {
    async firstReachable(_requestId: string, primary: string) {
      const response = await listRelays();
      return {
        brokerUrl: primary,
        relayJson: JSON.stringify(response),
        keyId: response.key_id ?? '',
        // Go's brokerapi verifies the relay list before returning it; a failed
        // verification is a rejected call, never an unverified response.
        signatureVerified: true,
      };
    },
    // The app hides these features in the desktop client (see isDesktop in the
    // mobile source); the rejections only guard against a stray call.
    sendTelemetryBatchJSON: () => Promise.reject(unavailable('Telemetry upload')),
    fetchManifestCandidate: () => Promise.reject(unavailable('The update manifest')),
    cancel: () => Promise.resolve(false),
  };
}

async function browserCopyText(text: string): Promise<void> {
  await navigator.clipboard.writeText(text);
}

export function installNativeModules(): void {
  const service = window.go?.vpnservice?.Service;
  const runtime = window.runtime;

  if (service == null || runtime == null) {
    NativeModules.OpenRungBroker = brokerModule(async () => previewRelayList());
    NativeModules.OpenRungDesktop = {
      appVersion: __APP_VERSION__,
      getProxyInfo: async () => previewProxyInfo(),
      copyText: browserCopyText,
      ...previewTunnelMode(),
    };
    return;
  }

  NativeModules.OpenRungVpn = {
    prepare: () => service.Prepare(),
    // The Go binding takes plain strings; the contract's nulls map to "".
    connect: (brokerUrl: string, targetCountry: string | null, targetRelayId: string | null) =>
      service.Connect(brokerUrl, targetCountry ?? '', targetRelayId ?? ''),
    disconnect: () => service.Disconnect(),
    getState: () => service.GetState(),
    getIdentity: () => service.GetIdentity(),
    setSplitTunnelConfig: (configJson: string) => service.SetSplitTunnelConfig(configJson),
  };
  runtime.EventsOn(STATE_EVENT, state => {
    DeviceEventEmitter.emit(STATE_EVENT, state);
  });

  NativeModules.OpenRungBroker = brokerModule(() => service.ListRelaysForDirectory());

  NativeModules.OpenRungDesktop = {
    appVersion: __APP_VERSION__,
    getProxyInfo: () => service.GetProxyInfo(),
    copyText: async (text: string) => {
      await runtime.ClipboardSetText(text);
    },
    getTunnelMode: () => service.GetTunnelMode(),
    setTunnelMode: (mode: string) => service.SetTunnelMode(mode),
  };

  // The webview would navigate itself; hand links to the user's real browser.
  Linking.openURL = (url: string) => {
    runtime.BrowserOpenURL(url);
    return Promise.resolve();
  };
}
