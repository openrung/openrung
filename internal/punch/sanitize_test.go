package punch

import "testing"

// TestSanitizePeersDropsRoutableHostsAndCaps is the regression test for the open
// UDP reflector: a peer must not be able to make Attempt spray probes at an
// arbitrary public victim, nor flood the sender with an unbounded candidate list.
func TestSanitizePeersDropsRoutableHosts(t *testing.T) {
	in := []Endpoint{
		{IP: "8.8.8.8", Port: 53, Kind: KindHost},        // public victim tagged as LAN → drop
		{IP: "192.168.1.5", Port: 1234, Kind: KindHost},  // real RFC1918 LAN → keep
		{IP: "127.0.0.1", Port: 4000, Kind: KindHost},    // loopback → keep
		{IP: "100.64.0.9", Port: 5000, Kind: KindHost},   // CGNAT client LAN, not globally routable → keep
		{IP: "203.0.113.7", Port: 6000, Kind: KindSrflx}, // public reflexive → keep (provenance upstream)
		{IP: "224.0.0.1", Port: 7000, Kind: KindSrflx},   // multicast → drop
		{IP: "0.0.0.0", Port: 8000, Kind: KindSrflx},     // unspecified → drop
		{IP: "10.0.0.1", Port: 0, Kind: KindHost},        // invalid port → drop
		{IP: "10.0.0.2", Port: 9000, Kind: "bogus"},      // unknown kind → drop
	}
	got := SanitizePeers(in)
	want := map[string]bool{
		"192.168.1.5:1234": true,
		"127.0.0.1:4000":   true,
		"100.64.0.9:5000":  true,
		"203.0.113.7:6000": true,
	}
	if len(got) != len(want) {
		t.Fatalf("SanitizePeers kept %d endpoints, want %d: %+v", len(got), len(want), got)
	}
	for _, e := range got {
		if !want[e.String()] {
			t.Errorf("unexpectedly kept %s (%s)", e.String(), e.Kind)
		}
	}
}

func TestSanitizePeersCapsEachKind(t *testing.T) {
	var in []Endpoint
	for i := 0; i < maxPunchPeers*3; i++ {
		in = append(in,
			Endpoint{IP: "10.1.1." + itoa(i), Port: 2000, Kind: KindHost},
			Endpoint{IP: "203.0.113." + itoa(i), Port: 3000, Kind: KindSrflx},
		)
	}
	got := SanitizePeers(in)
	var host, srflx int
	for _, e := range got {
		switch e.Kind {
		case KindHost:
			host++
		case KindSrflx:
			srflx++
		}
	}
	if host != maxPunchPeers || srflx != maxPunchPeers {
		t.Fatalf("cap not enforced: host=%d srflx=%d, want %d each", host, srflx, maxPunchPeers)
	}
}

// itoa avoids importing strconv for the tiny loop above; i stays < 24.
func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
