package vpnservice

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"openrung/desktop/config"
	"openrung/desktop/proxymode"
	"openrung/desktop/rulesets"
	"openrung/internal/client"
)

const splitTunnelDisabledSignature = "disabled"

var splitTunnelCountryOrder = []string{"ir", "cn"}

type ruleSetStageResult struct {
	directory string
	countries []string
	dropped   []string
	warnings  []error
}

// splitTunnelConfig is the persisted mobile-compatible bridge payload. Desktop
// parses excluded_packages for forward/contract compatibility but deliberately
// ignores it: a loopback proxy cannot exclude arbitrary OS processes.
type splitTunnelConfig struct {
	Version          int
	Enabled          bool
	BypassLAN        bool
	BypassCountries  []string
	ExcludedPackages []string
}

type splitTunnelWireConfig struct {
	Version          *int      `json:"version"`
	Enabled          *bool     `json:"enabled"`
	BypassLAN        *bool     `json:"bypass_lan"`
	BypassCountries  *[]string `json:"bypass_countries"`
	ExcludedPackages *[]string `json:"excluded_packages"`
}

// parseSplitTunnelConfig accepts the shared v1+ object schema, applies the
// mobile field defaults, and ignores unknown keys. Invalid/non-object JSON and
// versions older than v1 are rejected. A disabled config still parses; callers
// decide whether it has an effective routing rule.
func parseSplitTunnelConfig(raw string) (splitTunnelConfig, bool) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return splitTunnelConfig{}, false
	}
	var wire splitTunnelWireConfig
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return splitTunnelConfig{}, false
	}

	parsed := splitTunnelConfig{Version: 1, BypassLAN: true}
	if wire.Version != nil {
		parsed.Version = *wire.Version
	}
	if parsed.Version < 1 {
		return splitTunnelConfig{}, false
	}
	if wire.Enabled != nil {
		parsed.Enabled = *wire.Enabled
	}
	if wire.BypassLAN != nil {
		parsed.BypassLAN = *wire.BypassLAN
	}
	if wire.BypassCountries != nil {
		parsed.BypassCountries = append([]string(nil), (*wire.BypassCountries)...)
	}
	if wire.ExcludedPackages != nil {
		parsed.ExcludedPackages = append([]string(nil), (*wire.ExcludedPackages)...)
	}
	return parsed, true
}

// normalizedSplitTunnelCountryCodes filters unknown future country presets,
// de-duplicates case-insensitively, and returns the one canonical emission
// order shared with mobile: Iran, then China.
func normalizedSplitTunnelCountryCodes(codes []string) []string {
	requested := make(map[string]bool, len(codes))
	for _, code := range codes {
		requested[strings.ToLower(strings.TrimSpace(code))] = true
	}
	result := make([]string, 0, len(splitTunnelCountryOrder))
	for _, code := range splitTunnelCountryOrder {
		if requested[code] {
			result = append(result, code)
		}
	}
	return result
}

// splitTunnelEffectiveSignature changes only when desktop's emitted routing
// policy can change. Invalid, absent, disabled, and enabled-but-inert payloads
// are all the same baseline. excluded_packages is intentionally absent from
// the signature because desktop cannot emit Android's per-app TUN exclusion.
func splitTunnelEffectiveSignature(raw string) string {
	parsed, ok := parseSplitTunnelConfig(raw)
	if !ok || !parsed.Enabled {
		return splitTunnelDisabledSignature
	}
	countries := normalizedSplitTunnelCountryCodes(parsed.BypassCountries)
	if !parsed.BypassLAN && len(countries) == 0 {
		return splitTunnelDisabledSignature
	}
	return fmt.Sprintf("enabled|lan=%t|c=%s", parsed.BypassLAN, strings.Join(countries, ","))
}

// splitTunnelRulesSignature keys the rules actually handed to the config
// generator, i.e. after staging dropped any country whose rule-set pair failed
// validation. splitTunnelEffectiveSignature keys the user's *request*; the two
// disagree exactly when fail-toward-proxy dropped a preset, which is why a
// reapply compares this one before bouncing a working tunnel.
func splitTunnelRulesSignature(rules *client.SplitTunnelRules) string {
	if rules == nil {
		return splitTunnelDisabledSignature
	}
	return fmt.Sprintf(
		"enabled|lan=%t|c=%s|dir=%s",
		rules.BypassLAN,
		strings.Join(rules.BypassCountries, ","),
		rules.RuleSetDirectory,
	)
}

// makeSplitTunnelRules builds one immutable connection-pass snapshot after the
// service has staged/validated the requested country rule-set pairs. Passing
// only availableCountries is the fail-toward-proxy boundary: a missing pair silently
// disappears while LAN bypass and every other country remain usable.
func makeSplitTunnelRules(
	raw string,
	ruleSetDirectory string,
	availableCountries []string,
) *client.SplitTunnelRules {
	parsed, ok := parseSplitTunnelConfig(raw)
	if !ok || !parsed.Enabled {
		return nil
	}

	requested := normalizedSplitTunnelCountryCodes(parsed.BypassCountries)
	availableSet := make(map[string]bool, len(availableCountries))
	for _, country := range availableCountries {
		availableSet[strings.ToLower(strings.TrimSpace(country))] = true
	}
	countries := make([]string, 0, len(requested))
	for _, country := range requested {
		if availableSet[country] {
			countries = append(countries, country)
		}
	}
	if !parsed.BypassLAN && len(countries) == 0 {
		return nil
	}

	return &client.SplitTunnelRules{
		BypassLAN:           parsed.BypassLAN,
		BypassCountries:     countries,
		RuleSetDirectory:    ruleSetDirectory,
		ProxyDomainSuffixes: proxyProbeDomainSuffixes(),
	}
}

// proxyProbeDomainSuffixes derives the config generator's force-proxy pin from
// the actual health endpoints. This avoids a country geosite refresh silently
// diverting a CONNECTED probe onto the direct path.
func proxyProbeDomainSuffixes() []string {
	seen := make(map[string]bool, len(config.InternetProbeURLs))
	result := make([]string, 0, len(config.InternetProbeURLs))
	for _, raw := range config.InternetProbeURLs {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(parsed.Hostname())), ".")
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		result = append(result, host)
	}
	return result
}

func (s *Service) ruleSetStager() func(string, []string) ruleSetStageResult {
	if s.stageRuleSets != nil {
		return s.stageRuleSets
	}
	return func(directory string, requested []string) ruleSetStageResult {
		result := rulesets.Stage(directory, requested)
		return ruleSetStageResult{
			directory: result.Directory,
			countries: append([]string(nil), result.Countries...),
			dropped:   append([]string(nil), result.Dropped...),
			warnings:  append([]error(nil), result.Warnings...),
		}
	}
}

// splitTunnelSnapshot stages the selected country pairs and returns one
// immutable rule snapshot for a complete initial or recovery ladder pass. Any
// storage or asset failure drops toward the normal proxied route and is only a
// diagnostic; it never turns a split-tunnel preference into a failed connect.
func (s *Service) splitTunnelSnapshot() *client.SplitTunnelRules {
	s.mu.Lock()
	raw := s.splitTunnelRaw
	s.mu.Unlock()

	parsed, ok := parseSplitTunnelConfig(raw)
	if !ok || !parsed.Enabled {
		return nil
	}
	requested := normalizedSplitTunnelCountryCodes(parsed.BypassCountries)
	if len(requested) == 0 {
		return makeSplitTunnelRules(raw, "", nil)
	}

	cacheBase, err := os.UserCacheDir()
	if err != nil {
		s.appendLog("could not resolve the split-tunnel rule-set cache; country bypasses are disabled")
		return makeSplitTunnelRules(raw, "", nil)
	}
	result := s.ruleSetStager()(filepath.Join(cacheBase, "openrung", "rulesets"), requested)
	for _, warning := range result.warnings {
		if warning != nil {
			s.appendLog("split-tunnel rule-set warning: " + warning.Error())
		}
	}
	for _, country := range result.dropped {
		s.appendLog(strings.ToUpper(country) + " split-tunnel rules are unavailable; routing it through the relay")
	}
	return makeSplitTunnelRules(raw, result.directory, result.countries)
}

// SetSplitTunnelConfig is the Wails/mobile-compatible native bridge method.
// It always stores the raw payload, then reapplies it to a fully connected
// tunnel. A connecting/recovering flow is deliberately not interrupted: it
// reconciles itself once it settles (reladder for a recovery, promote for an
// initial connect), so a change made mid-connect is never silently dropped.
func (s *Service) SetSplitTunnelConfig(configJSON string) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	var persistErr error
	if s.store != nil {
		persistErr = s.store.SaveSplitTunnelConfig(configJSON)
		if persistErr != nil {
			s.appendLog("could not persist split-tunnel settings: " + persistErr.Error())
		}
	}

	s.mu.Lock()
	s.splitTunnelRaw = configJSON
	s.mu.Unlock()

	s.reapplySplitTunnelLocked()
	return persistErr
}

// reapplySplitTunnelDrift is the post-promote reconciliation hook. It must run
// on its own goroutine: it takes connectMu and can replace the connection whose
// ladder called it, and connectLockedWithSnapshot waits for that connection's
// goroutine to finish.
func (s *Service) reapplySplitTunnelDrift() {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.reapplySplitTunnelLocked()
}

// reapplySplitTunnelLocked requires connectMu. It reconnects a fully connected
// session when the stored preference no longer matches what that session was
// built from. It is a no-op when nothing is connected, when the request has not
// drifted, or when the request drifted but produces a byte-identical config.
func (s *Service) reapplySplitTunnelLocked() {
	requestedSig := s.splitTunnelEffectiveSignatureNow()

	// Stage while the proven tunnel remains active. Revalidate the same
	// connection afterwards: its supervisor can enter recovery concurrently,
	// and settings must never cancel a recovery that is waiting for the network.
	s.mu.Lock()
	conn := s.conn
	fullyConnected := conn != nil && s.core.status == StatusConnected && !conn.disconnecting && !conn.finalized
	drifted := fullyConnected && conn.splitTunnelRequestedSig != requestedSig
	s.mu.Unlock()
	if !drifted {
		return
	}
	rules := s.splitTunnelSnapshot()

	s.mu.Lock()
	if s.conn != conn || s.core.status != StatusConnected || conn.disconnecting || conn.finalized {
		s.mu.Unlock()
		return
	}
	// Record that this connection is now reconciled to the current request
	// whatever we decide below, so a dropped preset cannot make promote and this
	// path re-evaluate the same drift forever.
	conn.splitTunnelRequestedSig = requestedSig
	if splitTunnelRulesSignature(rules) == splitTunnelRulesSignature(conn.splitTunnel) {
		// The request changed but the emitted policy did not — e.g. the user
		// toggled off a country whose rule-set pair had already been dropped.
		// Bouncing a working tunnel for an identical config is pure downtime.
		conn.splitTunnel = rules
		s.mu.Unlock()
		return
	}
	brokerURL := conn.requestedBrokerURL
	targetCountry := conn.requestedTargetCountry
	targetRelayID := conn.requestedTargetRelayID
	var inheritedProxySnapshot *proxymode.Snapshot
	if conn.snapshotTaken {
		snapshot := conn.snapshot
		inheritedProxySnapshot = &snapshot
	}
	// Claim the old connection under the same lock its supervisor consults,
	// preventing recovery from starting between this qualification and cancel.
	conn.disconnecting = true
	s.mu.Unlock()

	s.appendLog("split-tunnel settings changed; reconnecting")
	s.connectLockedWithSnapshot(
		brokerURL,
		targetCountry,
		targetRelayID,
		rules,
		inheritedProxySnapshot,
		connectTriggerSettings,
	)
}

func (s *Service) splitTunnelEffectiveSignatureNow() string {
	s.mu.Lock()
	raw := s.splitTunnelRaw
	s.mu.Unlock()
	return splitTunnelEffectiveSignature(raw)
}

// connSplitTunnel reads the pass-immutable rule snapshot. reladder replaces the
// pointer between passes, so every candidate builder goes through this accessor
// rather than touching the field directly.
func (s *Service) connSplitTunnel(conn *connection) *client.SplitTunnelRules {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.splitTunnel
}
