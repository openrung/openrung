// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e && linux

package e2e

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"openrung/test/e2e/lab"
)

// The user connects and transfers data over access links of varying quality.
// Profiles with added latency need the netem qdisc and are skipped without it.
func TestConnectOverDegradedUserLink(t *testing.T) {
	shapes := map[string]lab.Shape{
		"lossy": {Loss: 5},
		"slow":  {Rate: "512kbit"},
	}
	for name, shape := range lab.Profiles {
		shapes[name] = shape
	}
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		shape := shapes[name]
		t.Run(name, func(t *testing.T) {
			if shape.Delay > 0 || shape.Jitter > 0 {
				needNetem(t)
			}
			scenario(t)
			must(t, lab.ShapeLink("gw-user", shape))

			client, via := connect(t)
			exitIP(t, client, lab.ExitAddr(via))
			size, err := client.Download("http://files.example/bytes/65536", 2*time.Minute)
			must(t, err)
			if size != 65536 {
				t.Fatalf("downloaded %d bytes over a %s link, want 65536", size, name)
			}
		})
	}
}

// A volunteer relays over a poor home uplink. Sessions through it are slow
// but must still work.
func TestVolunteerOnDegradedUplink(t *testing.T) {
	scenario(t)
	must(t, lab.ShapeLink("gw-vol", lab.Shape{Loss: 3, Rate: "1mbit"}))

	client, _ := connect(t, "-relay-label", "vol")
	exitIP(t, client, lab.GwVolAddr)
	size, err := client.Download("http://files.example/bytes/65536", 2*time.Minute)
	must(t, err)
	if size != 65536 {
		t.Fatalf("downloaded %d bytes through the volunteer, want 65536", size)
	}
}

// A rate limit on the user's link bounds what the tunnel can carry, which
// shows shaping really sits in the tunnel's path.
func TestRateLimitBoundsTunnelThroughput(t *testing.T) {
	scenario(t)
	client, _ := connect(t, "-relay-label", "relay-a")
	const size = 256 << 10
	url := fmt.Sprintf("http://files.example/bytes/%d", size)

	download := func() time.Duration {
		start := time.Now()
		got, err := client.Download(url, 2*time.Minute)
		must(t, err)
		if got != size {
			t.Fatalf("downloaded %d bytes, want %d", got, size)
		}
		return time.Since(start)
	}
	unshaped := download()
	must(t, lab.ShapeLink("gw-user", lab.Shape{Rate: "1mbit"}))
	shaped := download()

	// 256 KiB at 1 Mbit/s takes about two seconds.
	if shaped < 1500*time.Millisecond {
		t.Fatalf("download took %s at 1 Mbit/s (%s unshaped), want at least 1.5s", shaped, unshaped)
	}
	t.Logf("256 KiB: %s unshaped, %s at 1 Mbit/s", unshaped, shaped)
}
