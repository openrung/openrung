import type { DirectSetupStatus } from '../native/types';

export const LOCAL_SETUP_SCOPE =
  'This changes only this computer. It does not configure a router or NAT mapping, or open a cloud firewall or security group. Automatic checks Internet reachability separately; Direct only does not use the RelayHub reachability check.';

export const AUTOMATIC_FALLBACK =
  'If you use Automatic, volunteering remains available: it will try any distinct configured alternate port and then RelayHub.';

export function automaticPortSummary(alternatePort: number): string {
  return alternatePort === 443
    ? 'Automatic: TCP 443 only (the alternate matches the preferred port)'
    : `Automatic: TCP 443 first, then alternate direct port ${alternatePort}`;
}

export function directSetupTitle(status: DirectSetupStatus): string {
  switch (status.state) {
    case 'ready':
      return 'TCP 443 locally available';
    case 'needs_setup':
      return 'TCP 443 local setup needed';
    case 'unavailable':
      return 'TCP 443 local setup unavailable';
  }
}

export function directSetupActionFailure(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  return `Local setup was not changed: ${message}. ${AUTOMATIC_FALLBACK}`;
}

export function relayHubSetupNote(status: DirectSetupStatus): string | null {
  if (status.state === 'ready') {
    return 'No direct candidate was positively confirmed. A local bind problem, an unavailable RelayHub probe API, or a router, NAT mapping, OS firewall, or cloud firewall may be the reason. Automatic is using RelayHub; the console shows the candidate outcomes.';
  }
  return `${directSetupTitle(status)}: ${status.message} ${AUTOMATIC_FALLBACK}`;
}
