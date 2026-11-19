// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"strings"
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

// The user's network refuses one relay's address, silently or with an
// explicit rejection. The client must end up on a relay it can reach rather
// than on the refused one.
func TestUserNetworkBlocksOneRelay(t *testing.T) {
	for _, action := range []lab.Action{lab.Drop, lab.Reset, lab.Unreachable} {
		t.Run(string(action), func(t *testing.T) {
			scenario(t)
			_, err := lab.Block("gw-user", lab.Match{Dst: lab.RelayAAddr, Proto: "tcp"}, action)
			must(t, err)

			client, via := connect(t)
			if via == "relay-a" || via == "" {
				t.Fatalf("connected via %q, want a relay other than the blocked relay-a", via)
			}
			exitIP(t, client, lab.ExitAddr(via))
		})
	}
}

// Every relay with a public address is unreachable from the user's network;
// the volunteer published through the relay hub still is.
func TestUserNetworkBlocksAllDirectRelays(t *testing.T) {
	scenario(t)
	for _, addr := range []string{lab.RelayAAddr, lab.RelayBAddr} {
		_, err := lab.Block("gw-user", lab.Match{Dst: addr}, lab.Drop)
		must(t, err)
	}
	client, via := connect(t)
	if via != "vol" {
		t.Fatalf("connected via %q, want vol", via)
	}
	exitIP(t, client, lab.GwVolAddr)
}

// The user sits behind a firewall that only lets web traffic out. Relays on
// 443 work; the volunteer's high port at the relay hub does not.
func TestUserBehindWebOnlyFirewall(t *testing.T) {
	scenario(t)
	must(t, lab.AllowOnly("gw-user", []lab.Match{
		{Proto: "tcp", Port: 80},
		{Proto: "tcp", Port: 443},
		{Proto: "udp", Port: 53},
		{Proto: "tcp", Port: 53},
		// The lab broker listens on 8080; a real one is reached on 443.
		{Dst: lab.BrokerAddr, Proto: "tcp", Port: 8080},
	}, lab.Drop))

	client, via := connect(t)
	if via != "relay-a" && via != "relay-b" {
		t.Fatalf("connected via %q, want a relay on port 443", via)
	}
	exitIP(t, client, lab.ExitAddr(via))
	must(t, client.Stop())

	pinned := startClient(t, "-relay-label", "vol")
	if via, err := pinned.WaitConnected(90 * time.Second); err == nil {
		t.Fatalf("connected via %q through a port the firewall blocks", via)
	}
	if log := pinned.Log(); !strings.Contains(log, "All relay connection attempts failed") {
		t.Fatalf("client did not report exhausting its relays:\n%s", plain(log))
	}
}

// The broker cannot be reached: the client must give up with an error rather
// than hang, and connect normally once the broker is back.
func TestBrokerUnreachable(t *testing.T) {
	scenario(t)
	// The client also knows built-in broker addresses; in the lab their
	// names resolve to the internet stand-in, so refuse HTTPS to it as well.
	for _, dst := range []lab.Match{
		{Dst: lab.BrokerAddr, Proto: "tcp"},
		{Dst: lab.InetAddr, Proto: "tcp", Port: 443},
	} {
		_, err := lab.Block("gw-user", dst, lab.Reset)
		must(t, err)
	}

	client := startClient(t)
	if via, err := client.WaitConnected(120 * time.Second); err == nil {
		t.Fatalf("connected via %q without a broker", via)
	}
	must(t, client.WaitExit(30*time.Second))

	must(t, lab.ClearNode("gw-user"))
	client, via := connect(t)
	exitIP(t, client, lab.ExitAddr(via))
}

// The relay the user is connected through dies. The client notices through
// its health checks and moves the session to another relay.
func TestRelayDiesMidSession(t *testing.T) {
	slow(t)
	scenario(t)
	client, first := connect(t)
	exitIP(t, client, lab.ExitAddr(first))

	must(t, lab.Kill(first))
	err := lab.Eventually(4*time.Minute, func() error {
		if via := client.Via(); via != first {
			return nil
		}
		return errStillOn(first)
	})
	must(t, err)
	must(t, client.WaitLog("failed over from relay", 30*time.Second))
	second := client.Via()
	if second == "" {
		t.Fatalf("client did not report its new relay")
	}
	exitIP(t, client, lab.ExitAddr(second))
}

// The user's own link drops for a while and comes back. That is a local
// outage, not a dead relay: the client must not burn through its relays.
func TestUserLinkOutageIsNotARelayFailure(t *testing.T) {
	slow(t)
	scenario(t)
	client, via := connect(t, "-relay-label", "relay-a")

	must(t, lab.SetLinkDown("user", true))
	must(t, client.WaitLog("assuming a local outage, not failing over", 4*time.Minute))
	must(t, lab.SetLinkDown("user", false))

	err := lab.Eventually(60*time.Second, func() error {
		_, err := client.ExitIP(5 * time.Second)
		return err
	})
	must(t, err)
	exitIP(t, client, lab.ExitAddr(via))
	if strings.Contains(client.Log(), "failed over from relay") {
		t.Fatalf("client failed over during a local outage")
	}
}

// The user moves from one access network to another (home router to a mobile
// uplink) while the client is running. Without a restart, the client's proxy
// must carry traffic again over the new network.
func TestUserChangesNetwork(t *testing.T) {
	scenario(t)
	client, via := connect(t)
	exitIP(t, client, lab.ExitAddr(via))

	// The default route moves to the second uplink; the first goes dark.
	// Restoring the lab puts the route back.
	must(t, lab.In("user", "ip", "route", "replace", "default", "via", "10.20.0.1", "dev", "eth1"))
	must(t, lab.SetLinkDown("user", true))

	direct, err := lab.FetchDirect("user", "http://exit.example/ip", 10*time.Second)
	must(t, err)
	if direct != lab.GwMobileAddr {
		t.Fatalf("after the move the user exits at %s, want the mobile uplink %s", direct, lab.GwMobileAddr)
	}
	err = lab.Eventually(3*time.Minute, func() error {
		_, err := client.ExitIP(5 * time.Second)
		return err
	})
	must(t, err)
	exitIP(t, client, lab.ExitAddr(client.Via()))
	if !lab.Running(client.Name) {
		t.Fatalf("client exited after the network change")
	}
}

type errStillOn string

func (e errStillOn) Error() string { return "client is still on relay " + string(e) }
