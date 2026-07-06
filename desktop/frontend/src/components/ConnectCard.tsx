import { useVpnState } from '../state/useVpnState';
import type { ConnectionStatus } from '../native/types';

// Connection card, restyled to match the RN app: an inline status row (dot +
// STATUS + relay label), an OUTLINED connect button with a power icon that
// fills solid green once connected, and the fail-closed hint.

const STATUS_LABEL: Record<ConnectionStatus, string> = {
  disconnected: 'DISCONNECTED',
  preparing: 'PREPARING',
  connecting: 'CONNECTING',
  connected: 'CONNECTED',
  disconnecting: 'DISCONNECTING',
  failed: 'FAILED',
};

const PowerIcon = ({ color }: { color: string }) => (
  <svg
    width="18"
    height="18"
    viewBox="0 0 24 24"
    fill="none"
    stroke={color}
    strokeWidth="2.2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
    <line x1="12" y1="2" x2="12" y2="12" />
  </svg>
);

export function ConnectCard() {
  const { state, isWorking, isConnected, prepareAndConnect, disconnect } = useVpnState();
  const { status, relayLabel, lastError } = state.native;

  const active = isConnected || isWorking;
  const label = active ? 'DISCONNECT' : 'CONNECT';
  const onPrimary = () => {
    if (active) {
      void disconnect();
    } else {
      void prepareAndConnect();
    }
  };

  return (
    <div className="or-connect-card">
      <div className="or-connect-status-row">
        <span className={`or-connect-dot status-${status}`} />
        <span className="or-connect-status">{STATUS_LABEL[status] ?? status}</span>
        <span className="or-connect-relay">{relayLabel ?? 'auto relay'}</span>
      </div>

      {lastError != null && status === 'failed' && (
        <div className="or-connect-error">{lastError}</div>
      )}

      <button
        type="button"
        data-testid="connect-button"
        className={`or-connect-button ${isConnected ? 'is-connected' : ''} ${isWorking ? 'is-working' : ''}`}
        onClick={onPrimary}
        disabled={isWorking}
      >
        <span className="or-connect-fill" aria-hidden />
        <span className="or-connect-button-content">
          <PowerIcon color={isConnected ? '#061008' : '#65F58A'} />
          {label}
        </span>
      </button>

      <p className="or-connect-hint">
        {isConnected
          ? 'vpn is fail-closed — traffic is routed through the relay'
          : 'vpn is fail-closed — no traffic leaves until connected'}
      </p>
    </div>
  );
}
