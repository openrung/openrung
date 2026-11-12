// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/openrung/openrung/connectcore/client"
)

func bundledRuleSets(countries ...string) fstest.MapFS {
	bundled := fstest.MapFS{}
	for _, country := range countries {
		bundled["geosite-"+country+".srs"] = &fstest.MapFile{Data: []byte("site-" + country)}
		bundled["geoip-"+country+".srs"] = &fstest.MapFile{Data: []byte("ip-" + country)}
	}
	return bundled
}

func stagedRuleSets(t *testing.T, countries ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rulesets")
	if err := stageRuleSets(bundledRuleSets(countries...), dir); err != nil {
		t.Fatalf("stage rule sets: %v", err)
	}
	return dir
}

func TestSplitTunnelRulesFromTheBridgePayload(t *testing.T) {
	dir := stagedRuleSets(t, "ir", "cn")
	cases := []struct {
		name   string
		config string
		want   *client.SplitTunnelRules
	}{
		{"unparsable", `{`, nil},
		{"no version", `{"enabled":true,"bypass_lan":true}`, nil},
		{"disabled", `{"version":1,"enabled":false,"bypass_lan":true,"bypass_countries":["cn"]}`, nil},
		{"nothing bypassed", `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":[]}`, nil},
		{
			"lan only",
			`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":[]}`,
			&client.SplitTunnelRules{BypassLAN: true},
		},
		{
			"country without lan",
			`{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["cn"]}`,
			&client.SplitTunnelRules{BypassCountries: []string{"cn"}, RuleSetDirectory: dir},
		},
		{
			// Canonical order, duplicates and unknown codes dropped, and
			// Android's per-app exclusion never reaches the desktop builder.
			"normalized",
			`{"version":2,"enabled":true,"bypass_lan":true,"bypass_countries":["cn","xx","ir","cn"],"country_source":"auto","excluded_packages":["com.example"],"later_field":1}`,
			&client.SplitTunnelRules{BypassLAN: true, BypassCountries: []string{"ir", "cn"}, RuleSetDirectory: dir},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := splitTunnelRules(tc.config, dir)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rules = %+v, want %+v", got, tc.want)
			}
			if len(dropped) != 0 {
				t.Fatalf("unexpected dropped countries: %v", dropped)
			}
		})
	}
}

// A country whose rule sets are not on disk is left out and reported, and the
// rest of the config still applies: sing-box refuses to start on a missing
// rule-set file, so passing the country through would fail every connect.
func TestSplitTunnelRulesDropCountriesWithoutRuleSets(t *testing.T) {
	dir := stagedRuleSets(t, "ir", "cn")
	if err := os.Remove(filepath.Join(dir, "geoip-cn.srs")); err != nil {
		t.Fatal(err)
	}
	config := `{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["ir","cn"]}`

	got, dropped := splitTunnelRules(config, dir)
	want := &client.SplitTunnelRules{BypassLAN: true, BypassCountries: []string{"ir"}, RuleSetDirectory: dir}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(dropped, []string{"cn"}) {
		t.Fatalf("rules = %+v, dropped = %v", got, dropped)
	}

	// Nothing staged at all: only the LAN bypass survives.
	got, dropped = splitTunnelRules(config, "")
	want = &client.SplitTunnelRules{BypassLAN: true}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(dropped, []string{"ir", "cn"}) {
		t.Fatalf("rules = %+v, dropped = %v", got, dropped)
	}
}

func TestStageRuleSetsWritesAndRefreshes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "rulesets")
	if err := stageRuleSets(bundledRuleSets("cn"), dir); err != nil {
		t.Fatalf("stage: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	if read("geosite-cn.srs") != "site-cn" || read("geoip-cn.srs") != "ip-cn" {
		t.Fatal("staged rule sets do not match the bundled ones")
	}

	// A newer build's rule set replaces the staged one; nothing else is left
	// behind in the directory.
	updated := bundledRuleSets("cn")
	updated["geosite-cn.srs"] = &fstest.MapFile{Data: []byte("site-cn-v2")}
	if err := stageRuleSets(updated, dir); err != nil {
		t.Fatalf("restage: %v", err)
	}
	if read("geosite-cn.srs") != "site-cn-v2" {
		t.Fatal("restaging did not refresh a changed rule set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected only the two rule sets, got %v", entries)
	}

	if err := stageRuleSets(nil, dir); err == nil {
		t.Fatal("a build without bundled rule sets must report it")
	}
	if err := stageRuleSets(fstest.MapFS{}, dir); err == nil {
		t.Fatal("an empty bundle must report it")
	}
}

// Only a change to the rules the tunnel would run with counts; with no tunnel
// up, nothing reconnects either way.
func TestSetSplitTunnelConfigTracksTheEffectiveRules(t *testing.T) {
	s := New()
	s.ruleSetDir = stagedRuleSets(t, "cn")

	set := func(config string) {
		t.Helper()
		if err := s.SetSplitTunnelConfig(config); err != nil {
			t.Fatalf("SetSplitTunnelConfig: %v", err)
		}
	}
	set(`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn"]}`)
	if s.splitSignature != "1|cn" {
		t.Fatalf("signature = %q", s.splitSignature)
	}
	// Android's app list and the country's provenance change nothing here.
	set(`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn"],"country_source":"auto","excluded_packages":["com.example"]}`)
	if s.splitSignature != "1|cn" {
		t.Fatalf("signature = %q", s.splitSignature)
	}
	// Iran's rule sets are not staged in this test, so the preset is dropped
	// with a log line and the LAN bypass remains.
	set(`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["ir"]}`)
	if s.splitSignature != "1|" {
		t.Fatalf("signature = %q", s.splitSignature)
	}
	logged := false
	for _, line := range s.GetState().LogLines {
		if strings.Contains(line, "no rule sets for ir") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("dropped preset was not logged: %v", s.GetState().LogLines)
	}
	set(`{"version":1,"enabled":false}`)
	if s.splitSignature != "" {
		t.Fatalf("signature = %q", s.splitSignature)
	}
}
