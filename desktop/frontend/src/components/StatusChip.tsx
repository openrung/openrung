import type { ConnectionStatus } from '../native/types';

const LABELS: Record<ConnectionStatus, string> = {
  disconnected: 'Disconnected',
  preparing: 'Preparing…',
  connecting: 'Connecting…',
  connected: 'Connected',
  disconnecting: 'Disconnecting…',
  failed: 'Failed',
};

export function statusLabel(status: ConnectionStatus): string {
  return LABELS[status];
}

export function StatusChip({ status }: { status: ConnectionStatus }) {
  return (
    <span className={`status-chip status-${status}`} data-testid="status-chip">
      <span className="status-dot" />
      {statusLabel(status)}
    </span>
  );
}
