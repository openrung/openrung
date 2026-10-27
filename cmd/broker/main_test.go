package main

import (
	"crypto/ed25519"
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
	wssSeed, err := parseOptionalSeed("OPENRUNG_WSS_TICKET_SIGNING_SEED", " \t"+canonical+"\r\n", ed25519.SeedSize)
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
	if seed, err := parseOptionalSeed("OPENRUNG_WSS_TICKET_SIGNING_SEED", "", ed25519.SeedSize); err != nil || seed != nil {
		t.Fatalf("empty seed = %x, %v", seed, err)
	}
	raw := []byte(strings.Repeat("t", 32))
	seed, err := parseOptionalSeed("OPENRUNG_WSS_TICKET_SIGNING_SEED", base64.StdEncoding.EncodeToString(raw), ed25519.SeedSize)
	if err != nil || string(seed) != string(raw) {
		t.Fatalf("parsed seed = %x, %v", seed, err)
	}
	for _, value := range []string{"not-base64", base64.StdEncoding.EncodeToString(raw[:31])} {
		if _, err := parseOptionalSeed("OPENRUNG_WSS_TICKET_SIGNING_SEED", value, ed25519.SeedSize); err == nil {
			t.Fatalf("accepted invalid seed %q", value)
		}
	}
}

func TestNewSourceClassifierFromEnv(t *testing.T) {
	t.Setenv("OPENRUNG_SOURCE_ASN_DB", "")
	t.Setenv("OPENRUNG_DATACENTER_ASN_LIST", "")
	t.Setenv("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS", "192.0.2.0/24")
	if c, err := newSourceClassifier(); err != nil || c != nil {
		t.Fatalf("unset: %v %v, want disabled", c, err)
	}
	t.Setenv("OPENRUNG_SOURCE_ASN_DB", "/nonexistent/ip2asn.tsv")
	if _, err := newSourceClassifier(); err == nil {
		t.Fatal("accepted a table without an ASN list")
	}
	// Both set: starts even though the data cannot be loaded yet.
	t.Setenv("OPENRUNG_DATACENTER_ASN_LIST", "/nonexistent/datacenter-asns.txt")
	if c, err := newSourceClassifier(); err != nil || c == nil {
		t.Fatalf("configured: %v %v", c, err)
	}
	t.Setenv("OPENRUNG_SOURCE_ASN_MAX_AGE", "weekly")
	if _, err := newSourceClassifier(); err == nil {
		t.Fatal("accepted a malformed max age")
	}
	t.Setenv("OPENRUNG_SOURCE_ASN_MAX_AGE", "")
	t.Setenv("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS", "192.0.2.1")
	if _, err := newSourceClassifier(); err == nil {
		t.Fatal("accepted a malformed exempt CIDR")
	}
}

func TestParseRateLimits(t *testing.T) {
	t.Setenv("OPENRUNG_RATE_LIMIT_RELAY_LIST", "0.5:7")
	limits, err := parseRateLimits()
	if err != nil {
		t.Fatalf("parseRateLimits: %v", err)
	}
	if want := (broker.RateLimit{PerSecond: 0.5, Burst: 7}); limits.RelayList != want {
		t.Fatalf("relay list limit = %+v, want %+v", limits.RelayList, want)
	}
	if limits.Telemetry != (broker.RateLimit{}) {
		t.Fatalf("unset telemetry limit = %+v, want zero (default)", limits.Telemetry)
	}
	t.Setenv("OPENRUNG_RATE_LIMIT_ACCESS", "fast")
	if _, err := parseRateLimits(); err == nil || !strings.Contains(err.Error(), "OPENRUNG_RATE_LIMIT_ACCESS") {
		t.Fatalf("malformed limit error = %v, want it to name the variable", err)
	}
}

func TestParseRelayListLimits(t *testing.T) {
	for _, test := range []struct {
		defaultLimit, maxLimit string
		wantDefault, wantMax   int
		ok                     bool
	}{
		{ok: true},
		{defaultLimit: "3", maxLimit: "8", wantDefault: 3, wantMax: 8, ok: true},
		{defaultLimit: "8", maxLimit: "8", wantDefault: 8, wantMax: 8, ok: true},
		{defaultLimit: "9", maxLimit: "8"},
		{defaultLimit: "-1"},
		{maxLimit: "many"},
	} {
		t.Setenv("OPENRUNG_RELAY_LIST_DEFAULT_LIMIT", test.defaultLimit)
		t.Setenv("OPENRUNG_RELAY_LIST_MAX_LIMIT", test.maxLimit)
		gotDefault, gotMax, err := parseRelayListLimits()
		if (err == nil) != test.ok || gotDefault != test.wantDefault || gotMax != test.wantMax {
			t.Errorf("default=%q max=%q: got %d, %d, %v", test.defaultLimit, test.maxLimit, gotDefault, gotMax, err)
		}
	}
}

func TestParseWSSTicketBounds(t *testing.T) {
	for _, test := range []struct {
		ttl, streams string
		ok           bool
	}{
		{ok: true},
		{ttl: "90s", streams: "16", ok: true},
		{ttl: "1s"},
		{ttl: "24h"},
		{streams: "-1"},
		{streams: "1000000"},
	} {
		t.Setenv("OPENRUNG_WSS_TICKET_TTL", test.ttl)
		t.Setenv("OPENRUNG_WSS_TICKET_MAX_STREAMS", test.streams)
		if _, _, err := parseWSSTicketBounds(); (err == nil) != test.ok {
			t.Errorf("ttl=%q streams=%q: err = %v, want ok=%v", test.ttl, test.streams, err, test.ok)
		}
	}
}
