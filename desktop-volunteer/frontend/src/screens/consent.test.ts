import { describe, expect, it } from 'vitest';
import { canAcceptConsent, isConsentGateVisible } from './consent';

describe('isConsentGateVisible', () => {
  it('stays hidden before the first bridge snapshot arrives', () => {
    // The store default is consentAccepted=false, so an unhydrated store must
    // not flash the gate at volunteers who already accepted.
    expect(isConsentGateVisible(false, false)).toBe(false);
    expect(isConsentGateVisible(false, true)).toBe(false);
  });

  it('shows the gate once hydrated without consent', () => {
    expect(isConsentGateVisible(true, false)).toBe(true);
  });

  it('hides the gate after consent is accepted', () => {
    expect(isConsentGateVisible(true, true)).toBe(false);
  });
});

describe('canAcceptConsent', () => {
  it('keeps the accept button disabled until the IP-visibility box is checked', () => {
    expect(canAcceptConsent(false, false)).toBe(false);
  });

  it('enables the accept button once acknowledged', () => {
    expect(canAcceptConsent(true, false)).toBe(true);
  });

  it('never re-accepts already-granted consent', () => {
    expect(canAcceptConsent(true, true)).toBe(false);
  });
});
