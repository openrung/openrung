// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/openrung/openrung/connectcore/client"
)

func TestSplitTunnelShapesCandidateConfig(t *testing.T) {
	relay := relayAt("a", "JP", "Tokyo", "Japan", "203.0.113.10")
	s := New()

	if input := s.candidateConfigInput(relay, 7890); input.SplitTunnel != nil || input.DNSShape != client.DNSShapeTCPProxied {
		t.Fatalf("an engine with no split-tunnel rules must keep the plain config: %+v", input)
	}

	// LAN bypass alone changes neither the DNS shape nor the probe pins.
	s.SetSplitTunnel(&client.SplitTunnelRules{BypassLAN: true})
	input := s.candidateConfigInput(relay, 7890)
	if input.SplitTunnel == nil || !input.SplitTunnel.BypassLAN {
		t.Fatalf("LAN bypass did not reach the config input: %+v", input.SplitTunnel)
	}
	if input.DNSShape != client.DNSShapeTCPProxied || input.ProbeDomainSuffixes != nil {
		t.Fatalf("LAN bypass alone must keep the plain DNS shape: %+v", input)
	}

	rules := &client.SplitTunnelRules{
		BypassLAN:        true,
		BypassCountries:  []string{client.SplitTunnelCountryCN},
		RuleSetDirectory: "/rules",
	}
	s.SetSplitTunnel(rules)
	// The engine keeps its own copy.
	rules.BypassCountries[0] = client.SplitTunnelCountryIR
	input = s.candidateConfigInput(relay, 7890)
	if !reflect.DeepEqual(input.SplitTunnel.BypassCountries, []string{client.SplitTunnelCountryCN}) {
		t.Fatalf("caller mutation reached the engine's rules: %+v", input.SplitTunnel)
	}
	if input.DNSShape != client.DNSShapeDoHFailover {
		t.Fatalf("a country bypass needs the DoH failover DNS shape, got %v", input.DNSShape)
	}
	// Every host the engine probes through the tunnel must be pinned to the
	// proxy, or a bypass rule set that lists one would send the probe direct.
	for _, endpoint := range InternetProbeURLs {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatalf("parse probe URL %q: %v", endpoint, err)
		}
		if !slices.Contains(input.ProbeDomainSuffixes, parsed.Hostname()) {
			t.Fatalf("probe host %s is not pinned: %v", parsed.Hostname(), input.ProbeDomainSuffixes)
		}
	}
	if _, err := client.BuildSingBoxConfig(input); err != nil {
		t.Fatalf("the engine's split-tunnel input does not build: %v", err)
	}

	s.SetSplitTunnel(nil)
	if input := s.candidateConfigInput(relay, 7890); input.SplitTunnel != nil || input.DNSShape != client.DNSShapeTCPProxied {
		t.Fatalf("clearing the rules must restore the plain config: %+v", input)
	}
}

// A mobile host supplies its rules through MobileTunnelSettings; a value set
// here must not leak into its config.
func TestSplitTunnelIgnoredForMobileHosts(t *testing.T) {
	s := New()
	s.Mobile = &MobileHost{}
	s.SetSplitTunnel(&client.SplitTunnelRules{BypassLAN: true})
	if input := s.candidateConfigInput(relayAt("a", "JP", "Tokyo", "Japan", "203.0.113.10"), 0); input.SplitTunnel != nil {
		t.Fatalf("mobile host config picked up the desktop split-tunnel rules: %+v", input.SplitTunnel)
	}
}
