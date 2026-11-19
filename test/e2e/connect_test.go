// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

// A user behind a home router discovers relays from the broker and browses
// through one with a public address.
func TestConnectThroughDirectRelay(t *testing.T) {
	scenario(t)
	direct, err := lab.FetchDirect("user", "http://exit.example/ip", 10*time.Second)
	must(t, err)
	if direct != lab.GwUserAddr {
		t.Fatalf("without the tunnel the user exits at %s, want the home router %s", direct, lab.GwUserAddr)
	}

	client, via := connect(t, "-relay-label", "relay-a")
	if via != "relay-a" {
		t.Fatalf("connected via %q, want relay-a", via)
	}
	exitIP(t, client, lab.RelayAAddr)

	body, err := client.Fetch("https://www.example.org/hello", 20*time.Second)
	must(t, err)
	if want := "lab internet: www.example.org/hello"; body != want {
		t.Fatalf("HTTPS through the tunnel returned %q, want %q", body, want)
	}
}

// A volunteer behind a home router has no reachable port, so it publishes
// itself through the relay hub. The user's traffic reaches it through the
// hub (no direct path is attempted here) and leaves from the volunteer's
// home connection.
func TestConnectThroughVolunteerBehindNAT(t *testing.T) {
	scenario(t)
	client, via := connect(t, "-relay-label", "vol", "-punch=false")
	if via != "vol" {
		t.Fatalf("connected via %q, want vol", via)
	}
	exitIP(t, client, lab.GwVolAddr)
}
