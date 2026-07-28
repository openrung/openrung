import type { VolunteerState } from '../native/types';

/**
 * The public-facing route result shown once the relay is online. A tunnel's
 * broker endpoint is deliberately not described as a local direct endpoint.
 */
export function connectionResult(
  state: Pick<VolunteerState, 'transport' | 'publicEndpoint'>,
): string | null {
  if (state.transport === 'direct') {
    return state.publicEndpoint === ''
      ? 'Direct connection'
      : `Direct on ${state.publicEndpoint}`;
  }
  if (state.transport === 'tunnel') {
    return 'Via RelayHub';
  }
  return null;
}
