// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/punchcore"

	"openrung/test/e2e/lab"
)

// punched asserts that the client reports a direct path to a volunteer
// relay, punched through both NATs, reaching the volunteer at peer.
func punched(t *testing.T, client *lab.Client, label, peer string) {
	t.Helper()
	want := fmt.Sprintf("punched direct path to %s (peer %s, nat %s)", relayID(t, label), peer, punchcore.ClassEIM)
	if log := client.Log(); !strings.Contains(log, want) {
		t.Fatalf("client did not report %q:\n%s", want, plain(log))
	}
}

// notPunched asserts that the client tried to punch, gave up with the given
// reason and kept the relay hub's path.
func notPunched(t *testing.T, client *lab.Client, reason string) {
	t.Helper()
	log := client.Log()
	if strings.Contains(log, "punched direct path") {
		t.Fatalf("client reports a punched path where none can exist:\n%s", plain(log))
	}
	want := "punch failed (" + reason + "); using relay hub"
	if !strings.Contains(log, want) {
		t.Fatalf("client did not report %q:\n%s", want, plain(log))
	}
}

// The user and a volunteer are both behind ordinary home routers. With the
// relay hub's help they open a direct path through both routers, and the
// user's traffic flows over it: cutting the user off from the relay hub
// afterwards does not interrupt the session.
func TestPunchDirectPathToVolunteer(t *testing.T) {
	scenario(t)
	client, via := connect(t, "-relay-label", "vol")
	if via != "vol" {
		t.Fatalf("connected via %q, want vol", via)
	}
	punched(t, client, "vol", lab.GwVolAddr)
	exitIP(t, client, lab.GwVolAddr)

	_, err := lab.Block("gw-user", lab.Match{Dst: lab.HubAddr}, lab.Drop)
	must(t, err)
	exitIP(t, client, lab.GwVolAddr)
	size, err := client.Download("http://files.example/bytes/1048576", time.Minute)
	must(t, err)
	if size != 1<<20 {
		t.Fatalf("downloaded %d bytes over the punched path, want %d", size, 1<<20)
	}
}

// The volunteer sits behind two layers of NAT, a home router behind a
// carrier-grade NAT, both of which keep one public port per socket. A direct
// path can still be punched, to the carrier's public address.
func TestPunchDirectPathThroughCarrierGradeNAT(t *testing.T) {
	scenario(t)
	must(t, lab.Start(lab.Vol2Service()))
	must(t, lab.WaitListed(60*time.Second, append([]string{"vol2"}, lab.DefaultRelays...)...))

	client, _ := connect(t, "-relay-label", "vol2")
	punched(t, client, "vol2", lab.CGNAddr)
	_, err := lab.Block("gw-user", lab.Match{Dst: lab.HubAddr}, lab.Drop)
	must(t, err)
	exitIP(t, client, lab.CGNAddr)
}

// A router that picks a new public port for every destination makes the
// port the hub saw useless to the other side, so no direct path opens. When
// only one side is like that the attempt runs and times out; when both are,
// the hub does not even try. Either way the user is served through the hub.
func TestPunchFailsBehindSymmetricNAT(t *testing.T) {
	cases := []struct {
		name    string
		routers []string
		reason  string
	}{
		{"user", []string{"gw-user"}, "punch"},
		{"volunteer", []string{"gw-vol"}, "punch"},
		{"both", []string{"gw-user", "gw-vol"}, "declined:skip: both symmetric"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scenario(t)
			for _, router := range c.routers {
				must(t, lab.SetNAT(router, lab.NATSymmetric))
			}
			client := connectToVolunteer(t)
			notPunched(t, client, c.reason)
			exitIP(t, client, lab.GwVolAddr)
		})
	}
}

// The user's network drops all UDP. The client cannot learn its public
// mapping or reach the volunteer directly, and falls back to the hub's TCP
// path.
func TestPunchFailsWhenUserNetworkBlocksUDP(t *testing.T) {
	scenario(t)
	_, err := lab.Block("gw-user", lab.Match{Proto: "udp"}, lab.Drop)
	must(t, err)
	client := connectToVolunteer(t)
	notPunched(t, client, "punch")
	exitIP(t, client, lab.GwVolAddr)
}

// connectToVolunteer connects to vol, retrying while a NAT change on the
// volunteer's router makes it re-establish its tunnel to the relay hub.
func connectToVolunteer(t *testing.T) *lab.Client {
	t.Helper()
	var client *lab.Client
	err := lab.Eventually(3*time.Minute, func() error {
		client = startClient(t, "-relay-label", "vol")
		via, err := client.WaitConnected(60 * time.Second)
		if err == nil && via != "vol" {
			err = fmt.Errorf("connected via %q, want vol", via)
		}
		return err
	})
	must(t, err)
	return client
}
