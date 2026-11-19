// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"strings"
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

// wssRelay starts the Foundation relay that has a WSS front and waits until
// the broker lists it next to the default relays.
func wssRelay(t *testing.T) {
	t.Helper()
	must(t, lab.StartWSSRelay())
	must(t, lab.WaitListed(60*time.Second, append([]string{lab.FoundationRelay}, lab.DefaultRelays...)...))
}

// The user's network refuses a Foundation relay's address but lets web
// traffic to a CDN through. The client gives up on the direct path, fetches a
// ticket from the broker and reaches the same relay through its CDN front.
// Traffic still leaves from the relay's address.
func TestWSSFallbackThroughFront(t *testing.T) {
	for _, action := range []lab.Action{lab.Drop, lab.Reset} {
		t.Run(string(action), func(t *testing.T) {
			scenario(t)
			wssRelay(t)
			_, err := lab.Block("gw-user", lab.Match{Dst: lab.RelayFAddr, Proto: "tcp"}, action)
			must(t, err)
			edgeLog := lab.LogSize("internet")

			client, via := connect(t, "-relay-label", lab.FoundationRelay)
			if via != lab.FoundationRelay {
				t.Fatalf("connected via %q, want %s", via, lab.FoundationRelay)
			}
			must(t, client.WaitLog("connected through WSS front "+lab.WSSFrontID, 5*time.Second))
			exitIP(t, client, lab.RelayFAddr)

			body, err := client.Fetch("https://www.example.org/hello", 20*time.Second)
			must(t, err)
			if want := "lab internet: www.example.org/hello"; body != want {
				t.Fatalf("HTTPS through the WSS tunnel returned %q, want %q", body, want)
			}
			if log, _ := lab.ReadLog("internet", edgeLog); !strings.Contains(log, "lab edge: "+lab.WSSFrontHost) {
				t.Fatalf("the front saw no WebSocket request; internet log:\n%s", log)
			}
		})
	}
}

// The same relay is reachable directly. The client must use the direct path
// and never touch the CDN front: no ticket, no WebSocket.
func TestWSSUnusedWhenDirectWorks(t *testing.T) {
	scenario(t)
	wssRelay(t)
	edgeLog := lab.LogSize("internet")

	client, via := connect(t, "-relay-label", lab.FoundationRelay)
	if via != lab.FoundationRelay {
		t.Fatalf("connected via %q, want %s", via, lab.FoundationRelay)
	}
	exitIP(t, client, lab.RelayFAddr)
	if log := client.Log(); strings.Contains(log, "WSS front") {
		t.Fatalf("client used a WSS front although the direct path works:\n%s", plain(log))
	}
	if log, _ := lab.ReadLog("internet", edgeLog); strings.Contains(log, "lab edge:") {
		t.Fatalf("the front saw a request although the direct path works:\n%s", log)
	}
}
