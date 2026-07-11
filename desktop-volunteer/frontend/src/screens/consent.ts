// Pure presentation logic for the consent gate, kept out of the component so
// it can be unit tested without a DOM renderer (this app, like the sibling
// desktop app, has no react-testing-library dependency).

/**
 * The gate blocks the whole app until consent is accepted. It must NOT flash
 * while the store still holds the pre-hydration default (consentAccepted =
 * false), so it only shows once the first real bridge snapshot has landed.
 */
export function isConsentGateVisible(hydrated: boolean, consentAccepted: boolean): boolean {
  return hydrated && !consentAccepted;
}

/** The accept button stays disabled until the IP-visibility box is checked. */
export function canAcceptConsent(acknowledged: boolean, consentAccepted: boolean): boolean {
  return acknowledged && !consentAccepted;
}
