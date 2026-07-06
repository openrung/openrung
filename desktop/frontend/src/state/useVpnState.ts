/**
 * PORTED from openrung-mobile-app/src/state/useVpnState.ts. Only the import
 * paths change (../config → ../core/config); the hook body is unchanged, since
 * the desktop OpenRungVpn adapter presents the identical bridge contract.
 */
import { useCallback, useEffect } from 'react';
import { AppConfig } from '../core/config';
import { OpenRungVpn, subscribeVpnState } from '../native/OpenRungVpn';
import { applyNativeState, useAppState } from './store';
import type { AppState } from './store';

export interface VpnStateHook {
  state: AppState;
  /** preparing | connecting | disconnecting */
  isWorking: boolean;
  /** connected */
  isConnected: boolean;
  connect: (country?: string | null, relayId?: string | null) => Promise<void>;
  disconnect: () => Promise<void>;
  /**
   * Request OS consent via prepare(), then start the tunnel. Proceeds with the
   * start on ANY return from the consent flow (a declined prompt makes the
   * service fail and the status comes back through the store), so the prepare
   * result/failure is deliberately not gated on.
   */
  prepareAndConnect: (country?: string | null, relayId?: string | null) => Promise<void>;
}

export function useVpnState(): VpnStateHook {
  const state = useAppState();

  useEffect(() => {
    let mounted = true;
    OpenRungVpn.getState()
      .then(nativeState => {
        if (mounted) {
          applyNativeState(nativeState);
        }
      })
      .catch(() => {
        // Native state stays at the store default until the first event arrives.
      });
    const unsubscribe = subscribeVpnState(applyNativeState);
    return () => {
      mounted = false;
      unsubscribe();
    };
  }, []);

  const connect = useCallback(
    (country?: string | null, relayId?: string | null) =>
      OpenRungVpn.connect(AppConfig.DEFAULT_BROKER_URL, country ?? null, relayId ?? null),
    [],
  );

  const disconnect = useCallback(() => OpenRungVpn.disconnect(), []);

  const prepareAndConnect = useCallback(
    async (country?: string | null, relayId?: string | null) => {
      try {
        await OpenRungVpn.prepare();
      } catch {
        // Start the service on any consent-flow return.
      }
      await connect(country ?? null, relayId ?? null);
    },
    [connect],
  );

  const status = state.native.status;
  const isWorking =
    status === 'preparing' || status === 'connecting' || status === 'disconnecting';
  const isConnected = status === 'connected';

  return { state, isWorking, isConnected, connect, disconnect, prepareAndConnect };
}
