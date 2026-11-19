// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"os"
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

// The user routes the whole device through the tunnel instead of pointing
// applications at the local proxy. Ordinary, unproxied requests must leave
// through the relay.
func TestWholeDeviceTunnel(t *testing.T) {
	// The client gives its tunnel interface an IPv6 address, so the kernel
	// must have IPv6 enabled.
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		t.Skip("kernel has IPv6 disabled: the client cannot set up its tunnel interface")
	}
	scenario(t)
	client, via := connect(t, "--tun", "-relay-label", "relay-a")
	if via != "relay-a" {
		t.Fatalf("connected via %q, want relay-a", via)
	}
	err := lab.Eventually(20*time.Second, func() error {
		exit, err := lab.FetchDirect(client.Node, "http://exit.example/ip", 5*time.Second)
		if err != nil {
			return err
		}
		if exit != lab.RelayAAddr {
			return errExit(exit)
		}
		return nil
	})
	must(t, err)
	body, err := lab.FetchDirect(client.Node, "https://www.example.org/hello", 20*time.Second)
	must(t, err)
	if want := "lab internet: www.example.org/hello"; body != want {
		t.Fatalf("HTTPS over the tunnel interface returned %q, want %q", body, want)
	}
}

type errExit string

func (e errExit) Error() string {
	return "unproxied traffic exits at " + string(e) + ", not at the relay"
}
