// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package proxymode

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeDNSNetworksetup scripts per-service -getdnsservers output and records
// every mutating call.
type fakeDNSNetworksetup struct {
	servers map[string]string
	failSet map[string]bool
	calls   []string
}

func (f *fakeDNSNetworksetup) run(args ...string) ([]byte, error) {
	switch args[0] {
	case "-listallnetworkservices":
		return []byte("An asterisk (*) denotes that a network service is disabled.\nWi-Fi\nEthernet\n*Old Adapter\n"), nil
	case "-getdnsservers":
		return []byte(f.servers[args[1]]), nil
	case "-setdnsservers":
		f.calls = append(f.calls, strings.Join(args, " "))
		if f.failSet[args[1]] {
			return nil, errors.New("exit status 4")
		}
	}
	return nil, nil
}

func TestDNSSnapshotCapturesManualServersPerEnabledService(t *testing.T) {
	fake := &fakeDNSNetworksetup{servers: map[string]string{
		"Wi-Fi":    "There aren't any DNS Servers set on Wi-Fi.\n",
		"Ethernet": "9.9.9.9\n2620:fe::fe\n",
	}}
	snap, err := (&darwinDNSController{run: fake.run}).Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := []ServiceDNSState{
		{Name: "Wi-Fi"},
		{Name: "Ethernet", Servers: []string{"9.9.9.9", "2620:fe::fe"}},
	}
	if snap.Platform != "darwin" || !reflect.DeepEqual(snap.DNS, want) {
		t.Fatalf("snapshot = %+v; want darwin %+v", snap, want)
	}
	if len(snap.Services) != 0 || snap.Windows != nil {
		t.Fatalf("DNS snapshot carries proxy state: %+v", snap)
	}
}

// Output that is neither an address list nor the "none set" sentence must
// fail the snapshot: restoring a misparsed value would replace the user's
// servers with garbage.
func TestDNSSnapshotRefusesUnexpectedOutput(t *testing.T) {
	fake := &fakeDNSNetworksetup{servers: map[string]string{
		"Wi-Fi": "** Error: The parameters were not valid.\n",
	}}
	if _, err := (&darwinDNSController{run: fake.run}).Snapshot(); err == nil {
		t.Fatal("Snapshot accepted output that is not a DNS server list")
	}
}

func TestDNSSetPointsEachEnabledServiceAtServer(t *testing.T) {
	fake := &fakeDNSNetworksetup{}
	c := &darwinDNSController{run: fake.run}
	if err := c.Set("172.19.0.2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := []string{"-setdnsservers Wi-Fi 172.19.0.2", "-setdnsservers Ethernet 172.19.0.2"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Set calls = %v; want %v", fake.calls, want)
	}
	if err := c.Set("not-an-ip"); err == nil {
		t.Fatal("Set accepted a non-address server")
	}
}

func TestDNSRestoreWritesBackServersOrEmpty(t *testing.T) {
	fake := &fakeDNSNetworksetup{failSet: map[string]bool{"Wi-Fi": true}}
	c := &darwinDNSController{run: fake.run}
	err := c.Restore(Snapshot{Platform: "darwin", DNS: []ServiceDNSState{
		{Name: "Wi-Fi"},
		{Name: "Ethernet", Servers: []string{"9.9.9.9", "1.1.1.1"}},
	}})
	// A failing service is reported, but the others are still restored.
	if err == nil || !strings.Contains(err.Error(), "Wi-Fi") {
		t.Fatalf("Restore error = %v; want the Wi-Fi failure", err)
	}
	want := []string{"-setdnsservers Wi-Fi Empty", "-setdnsservers Ethernet 9.9.9.9 1.1.1.1"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Restore calls = %v; want %v", fake.calls, want)
	}
}

// Proxy and DNS snapshots share one persisted slot, and crash recovery hands
// the leftover to both controllers: each must leave the other's state alone.
func TestProxyAndDNSRestoresIgnoreEachOthersSnapshots(t *testing.T) {
	dnsFake := &fakeDNSNetworksetup{}
	proxySnap := Snapshot{Platform: "darwin", Services: []ServiceProxyState{{Name: "Wi-Fi"}}}
	if err := (&darwinDNSController{run: dnsFake.run}).Restore(proxySnap); err != nil {
		t.Fatalf("DNS Restore of a proxy snapshot: %v", err)
	}
	if len(dnsFake.calls) != 0 {
		t.Fatalf("DNS Restore of a proxy snapshot changed DNS: %v", dnsFake.calls)
	}

	proxyFake := &fakeNetworksetup{}
	dnsSnap := Snapshot{Platform: "darwin", DNS: []ServiceDNSState{{Name: "Wi-Fi"}}}
	if err := (&darwinController{run: proxyFake.run}).Restore(dnsSnap); err != nil {
		t.Fatalf("proxy Restore of a DNS snapshot: %v", err)
	}
	if muts := proxyFake.mutations(); len(muts) != 0 {
		t.Fatalf("proxy Restore of a DNS snapshot changed proxies: %v", muts)
	}
}
