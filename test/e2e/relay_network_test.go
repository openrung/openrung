// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

func relayID(t *testing.T, label string) string {
	t.Helper()
	relays, err := lab.ListedRelays()
	must(t, err)
	for _, relay := range relays {
		if relay.Label == label {
			return relay.ID
		}
	}
	t.Fatalf("broker does not list relay %s", label)
	return ""
}

// A relay goes away without saying goodbye. The broker stops handing it out
// when its lease runs out, and lists it again, under the same identity, when
// it comes back.
func TestRelayCrashAndRestart(t *testing.T) {
	scenario(t)
	before := relayID(t, "relay-b")

	must(t, lab.Kill("relay-b"))
	must(t, lab.WaitListed(60*time.Second, "relay-a", "vol"))

	definition, _ := lab.ServiceByName("relay-b")
	must(t, lab.Start(definition))
	must(t, lab.WaitListed(60*time.Second, lab.DefaultRelays...))
	if after := relayID(t, "relay-b"); after != before {
		t.Fatalf("relay-b came back as %s, want its old identity %s", after, before)
	}
	client, _ := connect(t, "-relay-label", "relay-b")
	exitIP(t, client, lab.RelayBAddr)
}

// A relay keeps serving but loses its path to the broker. Its lease runs out
// and it is listed again once the path returns.
func TestRelayLosesBroker(t *testing.T) {
	scenario(t)
	unblock, err := lab.Block("relay-b", lab.Match{Dst: lab.BrokerAddr}, lab.Drop)
	must(t, err)
	must(t, lab.WaitListed(60*time.Second, "relay-a", "vol"))

	must(t, unblock())
	must(t, lab.WaitListed(90*time.Second, lab.DefaultRelays...))
	client, _ := connect(t, "-relay-label", "relay-b")
	exitIP(t, client, lab.RelayBAddr)
}

// A volunteer's home connection drops and returns. The relay hub withdraws
// the volunteer and publishes it again when its tunnel is re-established.
func TestVolunteerLinkOutage(t *testing.T) {
	scenario(t)
	must(t, lab.SetLinkDown("vol", true))
	must(t, lab.WaitListed(3*time.Minute, "relay-a", "relay-b"))

	must(t, lab.SetLinkDown("vol", false))
	must(t, lab.WaitListed(3*time.Minute, lab.DefaultRelays...))
	client, _ := connect(t, "-relay-label", "vol")
	exitIP(t, client, lab.GwVolAddr)
}

// A volunteer's home router reboots and comes back with fresh address
// mappings, silently breaking the tunnel to the relay hub. The volunteer must
// end up published and usable again.
func TestVolunteerRouterReboot(t *testing.T) {
	scenario(t)
	must(t, lab.SetNAT("gw-vol", lab.NATSymmetric))
	var client *lab.Client
	err := lab.Eventually(3*time.Minute, func() error {
		client = startClient(t, "-relay-label", "vol")
		_, err := client.WaitConnected(30 * time.Second)
		return err
	})
	must(t, err)
	exitIP(t, client, lab.GwVolAddr)
}

// A volunteer sits behind two layers of NAT (a home router behind a
// carrier-grade NAT, both symmetric). It can only dial out, which is all the
// relay hub needs.
func TestVolunteerBehindCarrierGradeNAT(t *testing.T) {
	scenario(t)
	must(t, lab.SetNAT("cgn", lab.NATSymmetric))
	must(t, lab.SetNAT("gw-vol2", lab.NATSymmetric))
	must(t, lab.Start(lab.Vol2Service()))
	must(t, lab.WaitListed(60*time.Second, append([]string{"vol2"}, lab.DefaultRelays...)...))

	client, _ := connect(t, "-relay-label", "vol2")
	exitIP(t, client, lab.CGNAddr)
}

// A volunteer forwards port 443 on its home router and serves clients
// directly on the router's public address, with no relay hub in the path.
func TestVolunteerWithForwardedPort(t *testing.T) {
	scenario(t)
	undo, err := lab.PortForward("gw-vol", 443, lab.VolAddr+":443")
	must(t, err)
	t.Cleanup(func() { _ = undo() })
	must(t, lab.Start(lab.VolDirectService()))
	must(t, lab.WaitListed(60*time.Second, append([]string{"vol-direct"}, lab.DefaultRelays...)...))

	client, _ := connect(t, "-relay-label", "vol-direct")
	exitIP(t, client, lab.GwVolAddr)
}
