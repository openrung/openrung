// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

// Access-path telemetry (ADR-006 follow-up).
//
// Every connect that reaches the relay fetch records how per-user access
// went as an `access_outcome` attribute on its terminal connect event
// (connection_succeeded or connection_failed), and a successful connect also
// records `relay_source`: whether the relay it ended up on came from the
// access answer (a lease credential) or from the public list (the shared
// credential). Both are closed token sets, pinned by
// contract/vectors/access_outcome.json exactly like failure_reason is pinned
// by classification.json: the broker dashboard groups by them, so a token
// cannot change meaning after clients have shipped it. A connect that ends
// before the fetch (a local setup failure, a cancel) carries neither.

// access_outcome tokens.
const (
	// accessOutcomeGranted: the access answer was used as the candidate list.
	accessOutcomeGranted = "granted"
	// accessOutcomeNoKey: this installation has no key, so the public list
	// was used without asking.
	accessOutcomeNoKey = "no_key"
	// accessOutcomeNotRequested: a key exists but the connect's target cannot
	// be expressed to the access endpoint (a label-only target), so the
	// public list was used without asking.
	accessOutcomeNotRequested = "not_requested"
	// accessOutcomeUnsupported: the broker has no user/access endpoints.
	accessOutcomeUnsupported = "unsupported"
	// accessOutcomeFrontUnbound: the only reachable front cannot carry an
	// authenticated request (the endpoint-unbound no-SNI mode).
	accessOutcomeFrontUnbound = "front_unbound"
	// accessOutcomeDenied: the broker refused this installation (403); the
	// connect ends there.
	accessOutcomeDenied = "denied"
	// accessOutcomeUnapplied: the broker issued leases but no relay installed
	// them within the bounded retries (503).
	accessOutcomeUnapplied = "unapplied"
	// accessOutcomeRateLimited: the access request was rate limited (429).
	accessOutcomeRateLimited = "rate_limited"
	// accessOutcomeEmpty: the answer named no relay the ladder could use for
	// the target.
	accessOutcomeEmpty = "empty"
	// accessOutcomeError: any other failure of the access request.
	accessOutcomeError = "error"
	// accessOutcomeFallbackPublic: access was granted, every leased candidate
	// failed, and the public list carried the connect instead.
	accessOutcomeFallbackPublic = "fallback_public"
	// accessOutcomeEnrollmentRefused: the broker refused to enroll this
	// installation from the current (datacenter) network; the public list
	// was used.
	accessOutcomeEnrollmentRefused = "enrollment_refused"
)

// accessOutcomes is the closed set, in the order the vector file lists it.
var accessOutcomes = []string{
	accessOutcomeGranted, accessOutcomeNoKey, accessOutcomeNotRequested,
	accessOutcomeUnsupported, accessOutcomeFrontUnbound, accessOutcomeDenied,
	accessOutcomeUnapplied, accessOutcomeRateLimited, accessOutcomeEmpty,
	accessOutcomeError, accessOutcomeFallbackPublic, accessOutcomeEnrollmentRefused,
}

// relay_source tokens: where the connected relay's descriptor came from.
const (
	relaySourceAccess = "access"
	relaySourcePublic = "public"
)

var relaySources = []string{relaySourceAccess, relaySourcePublic}

// accessAttributes stamps the access-path attributes on a connect event's
// attribute map. relaySource is added only for a success (connected is
// true); a failed connect never reached a relay.
func (c *connection) accessAttributes(attrs map[string]string, connected bool) {
	if c.accessOutcome != "" {
		attrs["access_outcome"] = c.accessOutcome
	}
	if connected {
		if c.lease.valid() {
			attrs["relay_source"] = relaySourceAccess
		} else {
			attrs["relay_source"] = relaySourcePublic
		}
	}
}
