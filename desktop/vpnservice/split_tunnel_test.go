package vpnservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/desktop/persist"
	"openrung/desktop/proxymode"
	"openrung/internal/client"
	"openrung/internal/relay"
)

func TestParseSplitTunnelConfigMatchesMobileSchema(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want splitTunnelConfig
		ok   bool
	}{
		{
			name: "full v1 payload",
			raw:  `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["CN","ir"],"excluded_packages":["com.example.app"]}`,
			want: splitTunnelConfig{Version: 1, Enabled: true, BypassLAN: false, BypassCountries: []string{"CN", "ir"}, ExcludedPackages: []string{"com.example.app"}},
			ok:   true,
		},
		{
			name: "missing fields use mobile defaults and unknown keys are ignored",
			raw:  `{"enabled":true,"future":{"nested":true}}`,
			want: splitTunnelConfig{Version: 1, Enabled: true, BypassLAN: true},
			ok:   true,
		},
		{name: "disabled still parses", raw: `{"version":2,"enabled":false}`, want: splitTunnelConfig{Version: 2, BypassLAN: true}, ok: true},
		{name: "old version", raw: `{"version":0,"enabled":true}`, ok: false},
		{name: "wrong field type", raw: `{"enabled":"yes"}`, ok: false},
		{name: "array is not an object", raw: `[]`, ok: false},
		{name: "null is not an object", raw: `null`, ok: false},
		{name: "malformed", raw: `{`, ok: false},
		{name: "blank", raw: ``, ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSplitTunnelConfig(tc.raw)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseSplitTunnelConfig(%q) = %+v, %t; want %+v, %t", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSplitTunnelEffectiveSignatureIgnoresRawNoOpsAndPackages(t *testing.T) {
	disabled := splitTunnelEffectiveSignature("")
	for _, raw := range []string{
		`not json`,
		`{"version":1,"enabled":false,"bypass_lan":true,"bypass_countries":["ir"]}`,
		`{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["xx"],"excluded_packages":["com.example.app"]}`,
	} {
		if got := splitTunnelEffectiveSignature(raw); got != disabled {
			t.Fatalf("signature(%q) = %q, want disabled %q", raw, got, disabled)
		}
	}

	a := splitTunnelEffectiveSignature(`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn","IR","ir"],"excluded_packages":[]}`)
	b := splitTunnelEffectiveSignature(`{"future":1,"enabled":true,"bypass_countries":["ir","cn"],"excluded_packages":["ignored.on.desktop"]}`)
	if a != b || a != "enabled|lan=true|c=ir,cn" {
		t.Fatalf("equivalent signatures = %q and %q, want enabled|lan=true|c=ir,cn", a, b)
	}
	if got := splitTunnelEffectiveSignature(`{"enabled":true,"bypass_lan":false,"bypass_countries":["ir"]}`); got == a {
		t.Fatal("a real LAN routing change did not change the signature")
	}
}

func TestMakeSplitTunnelRulesKeepsOnlyStagedCountries(t *testing.T) {
	raw := `{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn","ir"],"excluded_packages":[]}`
	rules := makeSplitTunnelRules(raw, "/cache/rules", []string{"cn"})
	if rules == nil {
		t.Fatal("expected LAN + staged-country rules")
	}
	if !rules.BypassLAN || !reflect.DeepEqual(rules.BypassCountries, []string{"cn"}) || rules.RuleSetDirectory != "/cache/rules" {
		t.Fatalf("rules = %+v", rules)
	}
	if !reflect.DeepEqual(rules.ProxyDomainSuffixes, []string{"www.gstatic.com", "cp.cloudflare.com"}) {
		t.Fatalf("probe suffixes = %v", rules.ProxyDomainSuffixes)
	}

	if got := makeSplitTunnelRules(
		`{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["ir"]}`,
		"/cache/rules",
		nil,
	); got != nil {
		t.Fatalf("missing only country should degrade to baseline, got %+v", got)
	}
	if got := makeSplitTunnelRules(`{"version":1,"enabled":false}`, "/cache/rules", []string{"ir", "cn"}); got != nil {
		t.Fatalf("disabled config should produce nil rules, got %+v", got)
	}
}

func TestProxyProbeDomainSuffixesUsesConfiguredHostsAndDedupes(t *testing.T) {
	original := config.InternetProbeURLs
	config.InternetProbeURLs = []string{
		"https://Probe.Example:443/generate_204",
		"https://probe.example./other",
		"not a URL",
		"https://second.example/check",
	}
	t.Cleanup(func() { config.InternetProbeURLs = original })

	if got, want := proxyProbeDomainSuffixes(), []string{"probe.example", "second.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("proxyProbeDomainSuffixes() = %v, want %v", got, want)
	}
}

func TestApplyProxyPassesLANPreferenceToOptionController(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rules  *client.SplitTunnelRules
		bypass bool
	}{
		{name: "disabled sends all proxy-aware LAN traffic to sing-box", rules: nil, bypass: false},
		{name: "LAN preset bypasses before sing-box where required", rules: &client.SplitTunnelRules{BypassLAN: true}, bypass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := &optionCapturingProxy{}
			s := New()
			s.proxy = controller
			conn := &connection{splitTunnel: tc.rules}

			s.applyProxy(conn, 46685)

			if len(controller.options) != 1 || controller.options[0].BypassLAN != tc.bypass {
				t.Fatalf("SetWithOptions calls = %+v, want BypassLAN=%t", controller.options, tc.bypass)
			}
			if controller.legacySets != 0 {
				t.Fatalf("legacy Set called %d times despite OptionController support", controller.legacySets)
			}
		})
	}
}

func TestSetSplitTunnelConfigReappliesConnectedSessionToOriginalRequest(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	s.stageRuleSets = successfulRuleSetStager

	var configsMu sync.Mutex
	var configs [][]byte
	configDir := t.TempDir()
	s.writeConfig = func(data []byte) (string, error) {
		configsMu.Lock()
		defer configsMu.Unlock()
		configs = append(configs, append([]byte(nil), data...))
		return filepath.Join(configDir, fmt.Sprintf("config-%d.json", len(configs))), nil
	}

	if err := s.Connect("http://broker.example", "JP", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	raw := `{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["ir"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(raw); err != nil {
		t.Fatalf("SetSplitTunnelConfig: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	s.mu.Lock()
	conn := s.conn
	if conn == nil {
		s.mu.Unlock()
		t.Fatal("reapply left no live connection")
	}
	gotBroker := conn.requestedBrokerURL
	gotCountry := conn.requestedTargetCountry
	gotRelay := conn.requestedTargetRelayID
	rules := conn.splitTunnel
	s.mu.Unlock()
	if gotBroker != "http://broker.example" || gotCountry != "JP" || gotRelay != "relay-a" {
		t.Fatalf("reapply target = broker %q country %q relay %q", gotBroker, gotCountry, gotRelay)
	}
	if rules == nil || !rules.BypassLAN || !reflect.DeepEqual(rules.BypassCountries, []string{"ir"}) {
		t.Fatalf("reapply rules = %+v", rules)
	}
	if gotRaw, ok := s.store.LoadSplitTunnelConfig(); !ok || gotRaw != raw {
		t.Fatalf("persisted split config = %q, %v; want %q, true", gotRaw, ok, raw)
	}

	configsMu.Lock()
	configCount := len(configs)
	lastConfig := append([]byte(nil), configs[len(configs)-1]...)
	configsMu.Unlock()
	if configCount != 2 {
		t.Fatalf("config builds after one reapply = %d, want 2", configCount)
	}
	if tags := configRuleSetTags(t, lastConfig); !reflect.DeepEqual(tags, []string{"geosite-ir", "geoip-ir"}) {
		t.Fatalf("reapplied rule-set tags = %v", tags)
	}

	if err := s.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestSetSplitTunnelConfigEquivalentPayloadDoesNotReconnect(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	s.stageRuleSets = successfulRuleSetStager

	first := `{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":["cn","IR","ir"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(first); err != nil {
		t.Fatalf("initial SetSplitTunnelConfig: %v", err)
	}
	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	s.mu.Lock()
	before := s.conn
	s.mu.Unlock()
	second := `{"future":true,"enabled":true,"bypass_countries":["ir","cn"],"excluded_packages":["ignored.desktop.package"]}`
	if err := s.SetSplitTunnelConfig(second); err != nil {
		t.Fatalf("equivalent SetSplitTunnelConfig: %v", err)
	}
	s.mu.Lock()
	after := s.conn
	status := s.core.status
	s.mu.Unlock()
	if after != before || status != StatusConnected {
		t.Fatalf("no-op push replaced connection: before=%p after=%p status=%s", before, after, status)
	}
	if raw, ok := s.store.LoadSplitTunnelConfig(); !ok || raw != second {
		t.Fatalf("no-op raw payload was not persisted: %q, %v", raw, ok)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func TestSplitTunnelReapplyCarriesOriginalProxySnapshotAcrossRestoreFailure(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	original := proxymode.Snapshot{
		Platform: "windows",
		Windows: &proxymode.WindowsProxyState{
			ProxyEnable: true,
			ProxyServer: "10.0.0.1:3128",
		},
	}
	proxy := &reapplySnapshotProxy{current: original, restoreFailures: 1}
	s.proxy = proxy

	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	if err := s.SetSplitTunnelConfig(
		`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":[]}`,
	); err != nil {
		t.Fatalf("SetSplitTunnelConfig: %v", err)
	}
	waitForStatus(t, s, StatusConnected)

	if err := s.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)

	proxy.mu.Lock()
	snapshotCalls := proxy.snapshotCalls
	restores := append([]proxymode.Snapshot(nil), proxy.restores...)
	current := proxy.current
	proxy.mu.Unlock()
	if snapshotCalls != 1 {
		t.Fatalf("system proxy was snapshotted %d times, want exactly the original capture", snapshotCalls)
	}
	if len(restores) != 2 {
		t.Fatalf("proxy restores = %d, want failed reapply restore + final restore", len(restores))
	}
	for index, restored := range restores {
		if !reflect.DeepEqual(restored, original) {
			t.Fatalf("restore %d used %+v, want original %+v", index, restored, original)
		}
	}
	if !reflect.DeepEqual(current, original) {
		t.Fatalf("final system proxy = %+v, want original %+v", current, original)
	}
	if _, ok := s.store.LoadProxySnapshot(); ok {
		t.Fatal("successful final restore should clear the persisted proxy snapshot")
	}
}

func TestFailedSplitTunnelReapplyRetriesInheritedProxyRestore(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	original := proxymode.Snapshot{
		Platform: "windows",
		Windows: &proxymode.WindowsProxyState{
			ProxyEnable: true,
			ProxyServer: "10.0.0.1:3128",
		},
	}
	proxy := &reapplySnapshotProxy{current: original, restoreFailures: 1}
	s.proxy = proxy

	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	s.fetchRelays = func(context.Context, string, int, string, string) (discovery.Fetch, error) {
		return discovery.Fetch{}, errors.New("reapply broker unavailable")
	}
	if err := s.SetSplitTunnelConfig(
		`{"version":1,"enabled":true,"bypass_lan":true,"bypass_countries":[]}`,
	); err != nil {
		t.Fatalf("SetSplitTunnelConfig: %v", err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	proxy.mu.Lock()
	snapshotCalls := proxy.snapshotCalls
	restores := append([]proxymode.Snapshot(nil), proxy.restores...)
	current := proxy.current
	proxy.mu.Unlock()
	if snapshotCalls != 1 {
		t.Fatalf("system proxy was snapshotted %d times, want exactly the original capture", snapshotCalls)
	}
	if len(restores) != 2 {
		t.Fatalf("proxy restores = %d, want failed old restore + replacement cleanup retry", len(restores))
	}
	for index, restored := range restores {
		if !reflect.DeepEqual(restored, original) {
			t.Fatalf("restore %d used %+v, want original %+v", index, restored, original)
		}
	}
	if !reflect.DeepEqual(current, original) {
		t.Fatalf("final system proxy = %+v, want original %+v", current, original)
	}
	if _, ok := s.store.LoadProxySnapshot(); ok {
		t.Fatal("successful replacement cleanup should clear the persisted proxy snapshot")
	}
}

func TestRecoveryRefreshesConfigPersistedWhileConnecting(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	s.stageRuleSets = successfulRuleSetStager

	initial := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["ir"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(initial); err != nil {
		t.Fatalf("initial SetSplitTunnelConfig: %v", err)
	}

	fetchEntered := make(chan struct{})
	releaseFetch := make(chan struct{})
	var fetchCalls atomic.Int32
	s.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		if fetchCalls.Add(1) == 1 {
			close(fetchEntered)
			select {
			case <-releaseFetch:
			case <-ctx.Done():
				return discovery.Fetch{}, ctx.Err()
			}
		}
		return discovery.Fetch{BrokerURL: brokerURL, Response: listOf(fixtures...)}, nil
	}

	var configsMu sync.Mutex
	var configs [][]byte
	configDir := t.TempDir()
	s.writeConfig = func(data []byte) (string, error) {
		configsMu.Lock()
		defer configsMu.Unlock()
		configs = append(configs, append([]byte(nil), data...))
		return filepath.Join(configDir, fmt.Sprintf("config-%d.json", len(configs))), nil
	}
	var healthCalls atomic.Int32
	s.healthTick = time.Millisecond
	s.healthProbe = func(context.Context, int) error {
		if healthCalls.Add(1) <= int32(config.HealthFailureThreshold) {
			return errors.New("forced health loss")
		}
		return nil
	}
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-fetchEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connect never reached broker fetch")
	}
	s.mu.Lock()
	connectingConn := s.conn
	s.mu.Unlock()

	latest := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["cn"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(latest); err != nil {
		t.Fatalf("SetSplitTunnelConfig while connecting: %v", err)
	}
	s.mu.Lock()
	if s.conn != connectingConn || connectingConn.disconnecting {
		s.mu.Unlock()
		t.Fatal("settings update cancelled or replaced a connecting flow")
	}
	s.mu.Unlock()
	close(releaseFetch)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		configsMu.Lock()
		count := len(configs)
		configsMu.Unlock()
		if count >= 2 && s.GetState().Status == StatusConnected {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	configsMu.Lock()
	if len(configs) < 2 {
		configsMu.Unlock()
		t.Fatalf("recovery did not build a second config; status=%s logs=%v", s.GetState().Status, s.GetState().LogLines)
	}
	firstConfig := append([]byte(nil), configs[0]...)
	secondConfig := append([]byte(nil), configs[1]...)
	configsMu.Unlock()
	if tags := configRuleSetTags(t, firstConfig); !reflect.DeepEqual(tags, []string{"geosite-ir", "geoip-ir"}) {
		t.Fatalf("initial pass tags = %v, want Iran snapshot", tags)
	}
	if tags := configRuleSetTags(t, secondConfig); !reflect.DeepEqual(tags, []string{"geosite-cn", "geoip-cn"}) {
		t.Fatalf("recovery pass tags = %v, want latest China snapshot", tags)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

func successfulRuleSetStager(directory string, requested []string) ruleSetStageResult {
	return ruleSetStageResult{
		directory: "/staged/rules",
		countries: normalizedSplitTunnelCountryCodes(requested),
	}
}

func configRuleSetTags(t *testing.T, data []byte) []string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode sing-box config: %v", err)
	}
	route := decoded["route"].(map[string]any)
	rawRuleSets, _ := route["rule_set"].([]any)
	tags := make([]string, 0, len(rawRuleSets))
	for _, rawRuleSet := range rawRuleSets {
		ruleSet := rawRuleSet.(map[string]any)
		tags = append(tags, ruleSet["tag"].(string))
	}
	return tags
}

type optionCapturingProxy struct {
	legacySets int
	options    []proxymode.SetOptions
}

func (*optionCapturingProxy) Supported() bool { return true }

func (*optionCapturingProxy) Snapshot() (proxymode.Snapshot, error) {
	return proxymode.Snapshot{Platform: "test"}, nil
}

func (p *optionCapturingProxy) Set(string, int) error {
	p.legacySets++
	return nil
}

func (p *optionCapturingProxy) SetWithOptions(_ string, _ int, options proxymode.SetOptions) error {
	p.options = append(p.options, options)
	return nil
}

func (*optionCapturingProxy) Restore(proxymode.Snapshot) error { return nil }

type reapplySnapshotProxy struct {
	mu              sync.Mutex
	current         proxymode.Snapshot
	snapshotCalls   int
	restoreFailures int
	restores        []proxymode.Snapshot
}

func (*reapplySnapshotProxy) Supported() bool { return true }

func (p *reapplySnapshotProxy) Snapshot() (proxymode.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshotCalls++
	return p.current, nil
}

func (p *reapplySnapshotProxy) Set(host string, port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = proxymode.Snapshot{
		Platform: "windows",
		Windows: &proxymode.WindowsProxyState{
			ProxyEnable: true,
			ProxyServer: fmt.Sprintf("%s:%d", host, port),
		},
	}
	return nil
}

func (p *reapplySnapshotProxy) Restore(snapshot proxymode.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restores = append(p.restores, snapshot)
	if p.restoreFailures > 0 {
		p.restoreFailures--
		return errors.New("simulated proxy restore failure")
	}
	p.current = snapshot
	return nil
}

// slowProxyController stands in for a platform controller whose OS calls block
// (networksetup, the WinInet registry plus its change notification).
type slowProxyController struct {
	snap proxymode.Snapshot
}

func (f *slowProxyController) Supported() bool { return true }

func (f *slowProxyController) Snapshot() (proxymode.Snapshot, error) {
	time.Sleep(20 * time.Millisecond)
	return f.snap, nil
}

func (f *slowProxyController) Set(host string, port int) error {
	time.Sleep(20 * time.Millisecond)
	return nil
}

func (f *slowProxyController) Restore(snap proxymode.Snapshot) error { return nil }

// TestApplyProxyIsRaceFreeAgainstBridgeReads pins the locking discipline for the
// per-connection proxy state. applyProxy runs on the runConnect goroutine while
// currentProxySnapshot (user Connect) and SetSplitTunnelConfig read the same
// fields from the Wails bridge goroutine, so an unguarded write there is a real
// data race: a torn Snapshot is written back to the user's OS proxy settings.
// Only meaningful under -race, which CI runs for this package.
func TestApplyProxyIsRaceFreeAgainstBridgeReads(t *testing.T) {
	s := New()
	s.proxy = &slowProxyController{snap: proxymode.Snapshot{
		Platform: "darwin",
		Services: []proxymode.ServiceProxyState{{Name: "Wi-Fi"}},
	}}
	s.store = nil
	conn := &connection{}
	s.mu.Lock()
	s.conn = conn
	s.core.status = StatusConnected
	s.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		s.applyProxy(conn, 7890)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.currentProxySnapshot()
			time.Sleep(100 * time.Microsecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.connSplitTunnel(conn)
			time.Sleep(100 * time.Microsecond)
		}
	}()
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !conn.snapshotTaken || conn.snapshot.Platform != "darwin" || len(conn.snapshot.Services) != 1 {
		t.Fatalf("snapshot not captured intact: taken=%t snap=%+v", conn.snapshotTaken, conn.snapshot)
	}
}

// TestSplitTunnelChangeWhileConnectingAppliesAfterPromote covers the case where
// no recovery ever happens. A preference persisted between the initial ladder's
// snapshot and CONNECTED used to be dropped for the whole session, leaving the UI
// showing one policy while sing-box enforced another. promote must reconcile it.
func TestSplitTunnelChangeWhileConnectingAppliesAfterPromote(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	s.stageRuleSets = successfulRuleSetStager

	initial := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["ir"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(initial); err != nil {
		t.Fatalf("initial SetSplitTunnelConfig: %v", err)
	}

	fetchEntered := make(chan struct{})
	releaseFetch := make(chan struct{})
	var fetchCalls atomic.Int32
	s.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		if fetchCalls.Add(1) == 1 {
			close(fetchEntered)
			select {
			case <-releaseFetch:
			case <-ctx.Done():
				return discovery.Fetch{}, ctx.Err()
			}
		}
		return discovery.Fetch{BrokerURL: brokerURL, Response: listOf(fixtures...)}, nil
	}

	var configsMu sync.Mutex
	var configs [][]byte
	configDir := t.TempDir()
	s.writeConfig = func(data []byte) (string, error) {
		configsMu.Lock()
		defer configsMu.Unlock()
		configs = append(configs, append([]byte(nil), data...))
		return filepath.Join(configDir, fmt.Sprintf("config-%d.json", len(configs))), nil
	}
	// Healthy throughout: the reapply must come from promote, not from a recovery.
	s.healthTick = time.Millisecond
	s.healthProbe = func(context.Context, int) error { return nil }
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-fetchEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connect never reached broker fetch")
	}

	latest := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["cn"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(latest); err != nil {
		t.Fatalf("SetSplitTunnelConfig while connecting: %v", err)
	}
	close(releaseFetch)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		configsMu.Lock()
		count := len(configs)
		configsMu.Unlock()
		if count >= 2 && s.GetState().Status == StatusConnected {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	configsMu.Lock()
	count := len(configs)
	var lastConfig []byte
	if count > 0 {
		lastConfig = append([]byte(nil), configs[count-1]...)
	}
	configsMu.Unlock()
	if count < 2 {
		t.Fatalf("mid-connect change was never applied: %d config(s), status=%s logs=%v",
			count, s.GetState().Status, s.GetState().LogLines)
	}
	if tags := configRuleSetTags(t, lastConfig); !reflect.DeepEqual(tags, []string{"geosite-cn", "geoip-cn"}) {
		t.Fatalf("live config tags = %v, want the China snapshot the user asked for", tags)
	}
	waitForStatus(t, s, StatusConnected)

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}

// TestSplitTunnelReapplySkipsIdenticalEmittedConfig: after fail-toward-proxy has
// dropped a preset, turning that preset off changes the user's request but not
// the emitted policy. Bouncing a proven tunnel for a byte-identical config is
// pure downtime, so the reapply must recognise it and leave the session alone.
func TestSplitTunnelReapplySkipsIdenticalEmittedConfig(t *testing.T) {
	fixtures := []relay.Descriptor{relayAt("relay-a", "JP", "Tokyo", "Japan", "127.0.0.10")}
	s, _ := newLadderService(t, func() []relay.Descriptor { return fixtures })
	s.store = persist.NewInDir(t.TempDir())
	// China's rule-set pair never validates, so it is always dropped.
	s.stageRuleSets = func(directory string, requested []string) ruleSetStageResult {
		result := ruleSetStageResult{directory: "/staged/rules"}
		for _, country := range normalizedSplitTunnelCountryCodes(requested) {
			if country == "cn" {
				result.dropped = append(result.dropped, country)
				continue
			}
			result.countries = append(result.countries, country)
		}
		return result
	}

	var configsMu sync.Mutex
	var configs [][]byte
	configDir := t.TempDir()
	s.writeConfig = func(data []byte) (string, error) {
		configsMu.Lock()
		defer configsMu.Unlock()
		configs = append(configs, append([]byte(nil), data...))
		return filepath.Join(configDir, fmt.Sprintf("config-%d.json", len(configs))), nil
	}
	s.healthTick = time.Millisecond
	s.healthProbe = func(context.Context, int) error { return nil }
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	both := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["ir","cn"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(both); err != nil {
		t.Fatalf("SetSplitTunnelConfig: %v", err)
	}
	if err := s.Connect("http://broker.example", "", "relay-a"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForStatus(t, s, StatusConnected)
	s.mu.Lock()
	connected := s.conn
	s.mu.Unlock()

	// The user turns off the preset that was already unavailable.
	onlyIran := `{"version":1,"enabled":true,"bypass_lan":false,"bypass_countries":["ir"],"excluded_packages":[]}`
	if err := s.SetSplitTunnelConfig(onlyIran); err != nil {
		t.Fatalf("SetSplitTunnelConfig: %v", err)
	}

	s.mu.Lock()
	sameConn := s.conn == connected
	notDisconnecting := !connected.disconnecting
	reconciled := connected.splitTunnelRequestedSig == splitTunnelEffectiveSignature(onlyIran)
	s.mu.Unlock()
	if !sameConn || !notDisconnecting {
		t.Fatal("an identical emitted config must not replace the live connection")
	}
	if !reconciled {
		t.Fatal("the connection must record that it is reconciled to the new request")
	}
	configsMu.Lock()
	count := len(configs)
	configsMu.Unlock()
	if count != 1 {
		t.Fatalf("config builds = %d, want 1: a no-op change rebuilt the tunnel", count)
	}
	if status := s.GetState().Status; status != StatusConnected {
		t.Fatalf("status = %s, want still connected", status)
	}

	_ = s.Disconnect()
	waitForStatus(t, s, StatusDisconnected)
	waitIdle(t, s)
}
