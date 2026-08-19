package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"openrung/internal/broker"
)

func TestVersionInfo(t *testing.T) {
	// Injection and fallback resolution are internal/buildinfo's tests; this
	// guards the broker's wiring: its component name and embedded VERSION.
	want := "broker/" + strings.TrimSpace(baseVersion) + " revision="
	if got := versionInfo(); !strings.HasPrefix(got, want) {
		t.Fatalf("versionInfo() = %q, want prefix %q", got, want)
	}
}

// TestValidateAPIToken pins the startup credential-equality guard: the
// operational API token may not be any other broker credential, because each
// collision quietly hands operational-API holders a capability they were never
// meant to have (and, for the seeds, private signing key material).
func TestValidateAPIToken(t *testing.T) {
	const (
		apiToken     = "api-token"
		otherToken   = "other-token"
		signingSeed  = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
		ticketSeed   = "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M="
		unrelatedVar = "OPENRUNG_VOLUNTEER_TOKEN"
	)

	collisions := []struct {
		name  string
		peers []namedCredential
	}{
		{name: "volunteer token", peers: []namedCredential{{name: unrelatedVar, value: apiToken}}},
		{name: "foundation token", peers: []namedCredential{{name: "OPENRUNG_FOUNDATION_TOKEN", value: apiToken}}},
		{name: "dashboard token", peers: []namedCredential{{name: "OPENRUNG_DASHBOARD_TOKEN", value: apiToken}}},
		{name: "relay signing seed", peers: []namedCredential{{name: "OPENRUNG_RELAY_SIGNING_KEY", value: apiToken}}},
		{name: "wss ticket seed", peers: []namedCredential{{name: "OPENRUNG_WSS_TICKET_SIGNING_SEED", value: apiToken}}},
	}
	for _, test := range collisions {
		t.Run(test.name, func(t *testing.T) {
			err := validateAPIToken(apiToken, test.peers)
			if err == nil {
				t.Fatalf("accepted an API token equal to %s", test.peers[0].name)
			}
			// The operator must be told which value they reused.
			if !strings.Contains(err.Error(), test.peers[0].name) {
				t.Errorf("error %q does not name the colliding variable %s", err, test.peers[0].name)
			}
		})
	}

	distinct := []namedCredential{
		{name: unrelatedVar, value: otherToken},
		{name: "OPENRUNG_FOUNDATION_TOKEN", value: "foundation-token"},
		{name: "OPENRUNG_DASHBOARD_TOKEN", value: "dashboard-token"},
		{name: "OPENRUNG_RELAY_SIGNING_KEY", value: signingSeed},
		{name: "OPENRUNG_WSS_TICKET_SIGNING_SEED", value: ticketSeed},
	}
	if err := validateAPIToken(apiToken, distinct); err != nil {
		t.Fatalf("rejected a distinct API token: %v", err)
	}

	// An unset API token disables the endpoint, so it collides with nothing —
	// including the unset peers an open broker legitimately leaves empty.
	unset := []namedCredential{
		{name: unrelatedVar, value: ""},
		{name: "OPENRUNG_FOUNDATION_TOKEN", value: ""},
		{name: "OPENRUNG_DASHBOARD_TOKEN", value: ""},
	}
	if err := validateAPIToken("", unset); err != nil {
		t.Fatalf("rejected an unset API token: %v", err)
	}
	if err := validateAPIToken(apiToken, unset); err != nil {
		t.Fatalf("an unset peer credential must not count as a collision: %v", err)
	}
}

// The parsers accept harmless formatting variants around base64 seeds. The
// collision guard must compare the parsed material so those variants cannot
// hide that the API token discloses a signing seed.
func TestValidateAPITokenRejectsEquivalentSigningSeedEncodings(t *testing.T) {
	raw := []byte(strings.Repeat("s", 32))
	canonical := base64.StdEncoding.EncodeToString(raw)

	relaySeed, err := broker.ParseSigningSeed(canonical[:8] + "\n" + canonical[8:])
	if err != nil {
		t.Fatalf("parse relay seed with newline: %v", err)
	}
	wssSeed, err := parseOptionalWSSTicketSeed(" \t" + canonical + "\r\n")
	if err != nil {
		t.Fatalf("parse WSS seed with surrounding whitespace: %v", err)
	}

	for _, credential := range []namedCredential{
		canonicalSeedCredential("OPENRUNG_RELAY_SIGNING_KEY", relaySeed),
		canonicalSeedCredential("OPENRUNG_WSS_TICKET_SIGNING_SEED", wssSeed),
	} {
		t.Run(credential.name, func(t *testing.T) {
			if err := validateAPIToken(canonical, []namedCredential{credential}); err == nil {
				t.Fatalf("accepted an API token that reveals the parsed %s material", credential.name)
			}
		})
	}
}

func TestParseOptionalWSSTicketSeed(t *testing.T) {
	if seed, err := parseOptionalWSSTicketSeed(""); err != nil || seed != nil {
		t.Fatalf("empty seed = %x, %v", seed, err)
	}
	raw := []byte(strings.Repeat("t", 32))
	seed, err := parseOptionalWSSTicketSeed(base64.StdEncoding.EncodeToString(raw))
	if err != nil || string(seed) != string(raw) {
		t.Fatalf("parsed seed = %x, %v", seed, err)
	}
	for _, value := range []string{"not-base64", base64.StdEncoding.EncodeToString(raw[:31])} {
		if _, err := parseOptionalWSSTicketSeed(value); err == nil {
			t.Fatalf("accepted invalid seed %q", value)
		}
	}
}

func TestParseRelayPageDiversity(t *testing.T) {
	for _, value := range []string{"", "1", "true", "yes", " On "} {
		if enabled, err := parseRelayPageDiversity(value); err != nil || !enabled {
			t.Errorf("parseRelayPageDiversity(%q) = (%v, %v), want enabled", value, enabled, err)
		}
	}
	for _, value := range []string{"0", "false", "no", "off", "Disabled", "none"} {
		if enabled, err := parseRelayPageDiversity(value); err != nil || enabled {
			t.Errorf("parseRelayPageDiversity(%q) = (%v, %v), want disabled", value, enabled, err)
		}
	}
	// A rollback typo must refuse to start, never silently stay enabled.
	for _, value := range []string{"banana", "enable", "flase"} {
		if _, err := parseRelayPageDiversity(value); err == nil {
			t.Errorf("parseRelayPageDiversity(%q) accepted an unrecognized value", value)
		}
	}
}
