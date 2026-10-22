// SPDX-License-Identifier: GPL-3.0-or-later

package relay

import (
	"bytes"
	"testing"
)

func TestNormalizeAccessHost(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"2001:db8:1::10", "2001:db8:1::10"},
		{" 2001:DB8:1:0:0:0:0:10 ", "2001:db8:1::10"},
	} {
		got, err := NormalizeAccessHost(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeAccessHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"203.0.113.7",        // IPv4
		"::ffff:203.0.113.7", // IPv4-mapped
		"[2001:db8::1]",      // bracketed
		"2001:db8::1%eth0",   // zoned
		"relay.example.com",  // a name
		"::1",                // loopback
		"::",                 // unspecified
		"fe80::1",            // link-local
		"fd00::1",            // unique local
		"ff02::1",            // multicast
	} {
		if got, err := NormalizeAccessHost(in); err == nil {
			t.Errorf("NormalizeAccessHost(%q) = %q, want an error", in, got)
		}
	}
}

// The access host is private registration material: adding it must not
// change the deployed openrung-relay-identity-v1 statement.
func TestAccessHostIsNotBoundByTheIdentityStatement(t *testing.T) {
	req := RegisterRequest{
		PublicHost: "203.0.113.7", PublicPort: 443, ClientID: "11111111-2222-4333-8444-555555555555",
		RealityPublicKey: "key", ShortID: "0123456789abcdef", ServerName: "www.example.com",
		Flow: FlowVision, ExitMode: ExitModeDirect, MaxSessions: 75, MaxMbps: 100,
	}
	withAccess := req
	withAccess.AccessHost = "2001:db8:1::10"
	const expires = "2026-10-22T00:00:00Z"
	if !bytes.Equal(IdentityStatement(req, expires), IdentityStatement(withAccess, expires)) {
		t.Fatal("access_host changed the identity statement")
	}
}
