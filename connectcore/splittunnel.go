// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"net/url"

	"github.com/openrung/openrung/connectcore/client"
)

// SetSplitTunnel selects the split-tunnel rules subsequent tunnel configs are
// built with; nil turns split tunneling off. The caller has already decided
// policy and verified the rule-set files (see client.SplitTunnelRules). A live
// connection keeps the config it started with, so a host that wants a change
// applied at once reconnects.
//
// This is the desktop hosts' input. A mobile host supplies its rules through
// MobileTunnelSettings instead, and the engine then ignores this value.
func (s *Engine) SetSplitTunnel(rules *client.SplitTunnelRules) {
	var copied *client.SplitTunnelRules
	if rules != nil {
		copied = &client.SplitTunnelRules{
			BypassLAN:        rules.BypassLAN,
			BypassCountries:  append([]string(nil), rules.BypassCountries...),
			ExcludedPackages: append([]string(nil), rules.ExcludedPackages...),
			RuleSetDirectory: rules.RuleSetDirectory,
		}
	}
	s.mu.Lock()
	s.splitTunnel = copied
	s.mu.Unlock()
}

// applySplitTunnel adds the host's split-tunnel rules to a candidate's config
// input. The stored rules are never modified after SetSplitTunnel, so the
// builder can share them.
func (s *Engine) applySplitTunnel(input *client.SingBoxConfigInput) {
	if s.Mobile != nil {
		return
	}
	s.mu.Lock()
	rules := s.splitTunnel
	s.mu.Unlock()
	if rules == nil {
		return
	}
	input.SplitTunnel = rules
	if len(rules.BypassCountries) > 0 {
		// A country bypass needs the DoH failover DNS shape, and its
		// probe-priority pins must cover the hosts this engine probes: a
		// bypassed probe would report on the direct path, not the tunnel.
		input.DNSShape = client.DNSShapeDoHFailover
		input.ProbeDomainSuffixes = internetProbeHosts()
	}
}

// internetProbeHosts returns the hostnames of InternetProbeURLs.
func internetProbeHosts() []string {
	hosts := make([]string, 0, len(InternetProbeURLs))
	for _, endpoint := range InternetProbeURLs {
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Hostname() != "" {
			hosts = append(hosts, parsed.Hostname())
		}
	}
	return hosts
}
