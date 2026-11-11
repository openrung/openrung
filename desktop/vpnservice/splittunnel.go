// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/client"
)

// splitTunnelConfig is the bridge contract's split-tunnel payload
// (openrung-mobile-app docs/CONTRACT.md §3), reduced to what the desktop host
// applies. excluded_packages is Android's per-app exclusion and has no desktop
// counterpart. country_source is not consulted: the app re-derives an automatic
// country before every connect and pushes the result, and no tunnel outlives
// the app here.
type splitTunnelConfig struct {
	Version         int      `json:"version"`
	Enabled         bool     `json:"enabled"`
	BypassLAN       bool     `json:"bypass_lan"`
	BypassCountries []string `json:"bypass_countries"`
}

// connectRequest is the last target the app asked for, kept so a split-tunnel
// change can reconnect to it.
type connectRequest struct {
	brokerURL, targetCountry, targetRelayID string
}

// SetSplitTunnelConfig is Wails-bound. It applies the app's split-tunnel
// config to later connects and, when a tunnel is up and the rules it runs
// with changed, reconnects to the same target. A config that cannot be
// applied — unparsable, disabled, or naming a country whose rule sets are not
// on disk — degrades toward the full tunnel and never fails a connect.
func (s *Service) SetSplitTunnelConfig(configJSON string) error {
	rules, dropped := splitTunnelRules(configJSON, s.ruleSetDir)
	for _, country := range dropped {
		s.appendLog("split tunneling: no rule sets for " + country + "; not bypassing it")
	}
	signature := splitTunnelSignature(rules)

	s.splitMu.Lock()
	if signature == s.splitSignature {
		s.splitMu.Unlock()
		return nil
	}
	s.splitSignature = signature
	s.engine.SetSplitTunnel(rules)
	request := s.lastConnect
	s.splitMu.Unlock()

	if request == nil || s.engine.State().Status != connectcore.StatusConnected {
		return nil
	}
	s.appendLog("split tunneling changed; reconnecting")
	return s.engine.Connect(request.brokerURL, request.targetCountry, request.targetRelayID)
}

// splitTunnelRules turns the payload into builder rules, or nil when nothing
// is to be bypassed. dropped lists the requested countries left out because a
// rule-set file is missing from ruleSetDir.
func splitTunnelRules(configJSON, ruleSetDir string) (rules *client.SplitTunnelRules, dropped []string) {
	var config splitTunnelConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil || config.Version < 1 || !config.Enabled {
		return nil, nil
	}
	var countries []string
	// Canonical order, each country once, unknown codes ignored.
	for _, country := range client.SplitTunnelSupportedCountries {
		if !slices.Contains(config.BypassCountries, country) {
			continue
		}
		if !ruleSetsPresent(ruleSetDir, country) {
			dropped = append(dropped, country)
			continue
		}
		countries = append(countries, country)
	}
	if !config.BypassLAN && len(countries) == 0 {
		return nil, dropped
	}
	rules = &client.SplitTunnelRules{BypassLAN: config.BypassLAN, BypassCountries: countries}
	if len(countries) > 0 {
		rules.RuleSetDirectory = ruleSetDir
	}
	return rules, dropped
}

func ruleSetsPresent(dir, country string) bool {
	if dir == "" {
		return false
	}
	for _, kind := range []string{"geosite-", "geoip-"} {
		info, err := os.Stat(filepath.Join(dir, kind+country+".srs"))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// splitTunnelSignature identifies the rules by what they change in the
// generated config.
func splitTunnelSignature(rules *client.SplitTunnelRules) string {
	if rules == nil {
		return ""
	}
	lan := "0"
	if rules.BypassLAN {
		lan = "1"
	}
	return lan + "|" + strings.Join(rules.BypassCountries, ",")
}

// stageRuleSets copies the bundled rule sets into dir, where sing-box reads
// them by path. A file that already holds the same bytes is left alone.
func stageRuleSets(bundled fs.FS, dir string) error {
	if bundled == nil {
		return errors.New("this build bundles no rule sets")
	}
	names, err := fs.Glob(bundled, "*.srs")
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("this build bundles no rule sets")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		data, err := fs.ReadFile(bundled, name)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, name)
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
			continue
		}
		// Write-then-rename, so a crash never leaves a truncated rule set.
		tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
		if err != nil {
			return err
		}
		_, err = tmp.Write(data)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), target)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
	}
	return nil
}

// ruleSetDirectory is where the bundled rule sets are staged: beside the
// app's other per-install state.
func ruleSetDirectory() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "openrung", "rulesets"), nil
}
