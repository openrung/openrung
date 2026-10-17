package connectcore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/clienttelemetry"
)

// MobileHost opts into the mobile host contract. Assign before any engine use;
// do not mutate it afterwards. Platform must be Android or iOS, mode TUN, and
// Android must install a SocketProtector. Nil preserves desktop behavior.
// The host supplies OS mechanics; Engine still owns the entire ladder, session,
// heartbeat, recovery, and teardown policy.
type MobileHost struct {
	InstallID       string // existing native install UUID, preserved verbatim (including case)
	AppVersion      string
	PlatformVersion string // Android API level / iOS version for broker headers
	Runtime         MobileRuntime
	// Settings returns an owned snapshot of effective persisted settings and checks
	// local platform permission/availability. Called before each candidate, including
	// recovery and WSS retry; honor ctx and do not start a tunnel or perform remote I/O.
	Settings func(context.Context) (MobileTunnelSettings, error)
	// Outbox is required and borrowed, never closed by Engine. Open/migrate it once in the
	// process owner; stop legacy session/upload loops before lending it. Do not
	// also set TelemetryOutboxDirectory. Shutdown joins uploads before owner Close.
	Outbox *clienttelemetry.Outbox
	// Attributes returns current platform metadata (locale, network, device, OS).
	// Called outside engine/manager locks; return an owned map promptly. Identity
	// attributes below always win. Never perform network I/O in this callback.
	Attributes func() map[string]string
	// Key is the installation's identity key (ADR-006), required: the host
	// implements it over the Android Keystore / iOS Keychain (load-or-create,
	// non-exportable). See InstallationKey.
	Key InstallationKey
}

// MobileTunnelSettings contains only host-owned builder inputs. Relay credentials,
// inbound mode, bridge endpoints and protected-socket exclusions remain engine-owned.
// DNS is always the shared DoH failover shape; probes must use its priority pins.
type MobileTunnelSettings struct {
	TunnelIPv4Address   string
	TunnelIPv6Address   string
	DNSServers          []string
	MTU                 int
	LogLevel            string
	ProbeDomainSuffixes []string
	SplitTunnel         *client.SplitTunnelRules
	RouteFindProcess    bool
	ClashAPI            bool
}

// MobileRuntime owns libbox and local configuration validation. Preflight must
// parse/construct/close the supplied graph without launching or opening sockets
// (libbox CheckConfig). Engine checks direct AND bridge graphs before a remote
// failure can unlock WSS. Run returns at launched, and unwinds a partial launch
// before returning an error. Every Run gets a fresh reporter and run handle.
type MobileRuntime interface {
	Preflight(context.Context, []byte) error
	Run(context.Context, []byte, *RunTelemetry) (MobileTunnelRun, error)
}

// MobileTunnelRun is bound to one OS TUN owner and libbox service. WaitReady
// proves that THIS run's TUN and platform network settings are published; an
// HTTP response cannot prove readiness. VerifyPath performs fresh nonce DNS
// through the TUN's hijacked resolver, then HTTPS to a configured priority-pinned
// endpoint (TLS validation, no redirects/cache, 2xx only). Startup uses bounded
// retries; health uses one sweep. Preserve the shipping DNS chain budgets.
//
// Android binds probes to this run's VPN Network. iOS MUST use explicit
// createUDPSessionThroughTunnel/createTCPConnectionThroughTunnel or equivalent:
// ordinary provider-created sockets bypass its own TUN. Never substitute a
// physical-network HTTP client. Return evidence only after both stages pass.
//
// Methods honor cancellation and must finish when ctx ends. Unknown errors are
// LOCAL, including during health checks: one such failure terminates the session
// without recovery. This matches shipping awaitTunnelHealthFailure on both OSes.
// Only a classified remote path failure may return *RemotePathError; health
// applies the three-failure gate only to these errors. Adapters must preserve
// classification, including inner probe timeouts, rather than stringify errors.
// Stop joins platform callbacks and reports final counters before it returns;
// the reporter is retired afterwards. No callback may reenter Engine from Sink.
type MobileTunnelRun interface {
	TunnelRun
	WaitReady(context.Context) error
	VerifyPath(context.Context, VerificationPhase) (TunnelPathEvidence, error)
}

type VerificationPhase string

const (
	VerificationStartup VerificationPhase = "startup"
	VerificationHealth  VerificationPhase = "health"
)

type TunnelPath string

const (
	PathAndroidVPN        TunnelPath = "android_vpn_network"
	PathIOSProviderTunnel TunnelPath = "ios_provider_through_tunnel"
)

// TunnelPathEvidence is an explicit attestation from the platform transport,
// not inferred from HTTP success. Zero/physical-path/incomplete evidence fails
// locally. Each result belongs only to the run/method invocation that returns it.
type TunnelPathEvidence struct {
	Path        TunnelPath
	FreshDNS    bool
	PinnedHTTPS bool
}

// RemotePathError is the binding's allow-listed remote DNS/HTTPS failure. Stage
// must be "dns_probe" or "internet_probe". Permission, unavailable VPN Network,
// invalid provider/API state, engine stop and cancellation are never remote.
// An unknown stage or a bare timeout is local, even in VerificationHealth.
// Wrap a shipping allow-listed probe timeout here with its actual DNS/HTTPS stage;
// propagate cancellation of the supplied context unchanged instead.
type RemotePathError struct {
	Stage string
	Err   error
}

func (e *RemotePathError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *RemotePathError) Unwrap() error { return e.Err }

func (s *Engine) validateMobile() error {
	h := s.Mobile
	if h == nil {
		return nil
	}
	if !s.tunMode() || (s.Platform != brokerapi.PlatformAndroid && s.Platform != brokerapi.PlatformIOS) {
		return errors.New("mobile host requires Android/iOS TUN mode")
	}
	if s.PunchEstablisher == nil {
		return errors.New("mobile host requires its punch establisher even when punching is disabled")
	}
	if h.Runtime == nil || h.Settings == nil {
		return errors.New("mobile runtime and settings are required")
	}
	if s.TunnelRuntime != nil {
		return errors.New("mobile host must own the sole tunnel runtime")
	}
	if !clienttelemetry.ValidInstallID(h.InstallID) || strings.TrimSpace(h.AppVersion) == "" {
		return errors.New("mobile install UUID and app version are required")
	}
	if h.Outbox == nil {
		return errors.New("mobile host requires the process-owned persistent outbox")
	}
	if h.Key == nil {
		return errors.New("mobile host requires the installation key")
	}
	if s.TelemetryOutboxDirectory != "" {
		return errors.New("mobile host must lend the single process-owned outbox")
	}
	if s.Platform == brokerapi.PlatformAndroid && s.currentProtector() == nil {
		return errors.New("Android mobile host requires socket protection")
	}
	return nil
}

func (s *Engine) mobileConfig(ctx context.Context, input client.SingBoxConfigInput) (client.SingBoxConfigInput, error) {
	if err := s.validateMobile(); err != nil {
		return input, err
	}
	settings, err := s.Mobile.Settings(ctx)
	if err != nil {
		return input, err
	}
	if err := ctx.Err(); err != nil {
		return input, err
	}
	input.TunnelIPv4Address = settings.TunnelIPv4Address
	input.TunnelIPv6Address = settings.TunnelIPv6Address
	input.DNSServers = append([]string(nil), settings.DNSServers...)
	input.MTU = settings.MTU
	input.LogLevel = settings.LogLevel
	input.ProbeDomainSuffixes = append([]string(nil), settings.ProbeDomainSuffixes...)
	if rules := settings.SplitTunnel; rules != nil {
		copied := *rules
		copied.BypassCountries = append([]string(nil), rules.BypassCountries...)
		copied.ExcludedPackages = append([]string(nil), rules.ExcludedPackages...)
		input.SplitTunnel = &copied
	}
	input.RouteFindProcess = settings.RouteFindProcess
	input.ClashAPI = settings.ClashAPI
	input.DNSShape = client.DNSShapeDoHFailover
	input.BridgeOwnsOuterSocket = true
	ipv4 := input.TunnelIPv4Address
	if ipv4 == "" {
		ipv4 = client.DefaultTunnelIPv4Address
	}
	if _, err := client.TunnelDNSAddress(ipv4); err != nil {
		return input, err
	}
	if input.TunnelIPv6Address != "" {
		p, err := netip.ParsePrefix(input.TunnelIPv6Address)
		if err != nil || !p.Addr().Is6() {
			return input, errors.New("invalid mobile IPv6 TUN prefix")
		}
	}
	switch input.LogLevel {
	case "", "trace", "debug", "info", "warn", "error", "fatal", "panic":
	default:
		return input, errors.New("invalid mobile log level")
	}
	// Validate the same two shapes shipping ensureLocalTunnelPreconditions checks.
	for _, bridged := range []bool{false, true} {
		check := input
		if bridged {
			check.BridgeHost, check.BridgePort = "127.0.0.1", 1
		}
		config, err := client.BuildSingBoxConfig(check)
		if err != nil {
			return input, err
		}
		if err := s.Mobile.Runtime.Preflight(ctx, config); err != nil {
			return input, err
		}
		if err := ctx.Err(); err != nil {
			return input, err
		}
	}
	return input, nil
}

func (s *Engine) verifyMobilePath(ctx context.Context, res *candidateResult, phase VerificationPhase) (int64, error) {
	started := time.Now()
	evidence, err := res.mobileRun.VerifyPath(ctx, phase)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		var remote *RemotePathError
		if errors.As(err, &remote) && remote.Err != nil &&
			!errors.Is(err, context.Canceled) && !isSocketProtectionFailure(err) &&
			(remote.Stage == "dns_probe" || remote.Stage == "internet_probe") {
			return 0, remote
		}
		return 0, markLocalCandidateError("tunnel_probe", err)
	}
	expected := PathAndroidVPN
	if s.Platform == brokerapi.PlatformIOS {
		expected = PathIOSProviderTunnel
	}
	if evidence.Path != expected || !evidence.FreshDNS || !evidence.PinnedHTTPS {
		return 0, markLocalCandidateError("tunnel_probe", errors.New("missing through-tunnel fresh DNS and pinned HTTPS evidence"))
	}
	return time.Since(started).Milliseconds(), nil
}

// RunTelemetry is a capability for exactly one runtime attempt. It exposes no
// session/heartbeat/upload lifecycle. Methods are concurrent-safe, return false
// after retirement, and never transfer a stale callback to a successor. Keep it
// with the corresponding libbox run; do not look up a replacement via Engine.
// Counters preserve mobile's session high-water semantics across recovery.
type RunTelemetry struct {
	mu             sync.Mutex
	retired        bool
	manager        *clienttelemetry.Manager
	relayID        string
	sent, received int64
	hasTraffic     bool
}

func (r *RunTelemetry) retire() {
	if r != nil {
		r.mu.Lock()
		r.retired = true
		r.mu.Unlock()
	}
}
func (r *RunTelemetry) UpdateTraffic(sent, received int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired {
		return false
	}
	r.sent, r.received = max(r.sent, sent), max(r.received, received)
	r.hasTraffic = true
	r.manager.UpdateTraffic(sent, received)
	return true
}

// RecordApplicationConnections accepts already attributed/reduced tunneled flow
// counts, never destinations. The host excludes DNS (port 53) and its own package
// and drains its reducer before Stop returns. Counts are chunked to broker limits;
// shared outbox batching keeps per-app batch totals within the ingestion budget.
func (r *RunTelemetry) RecordApplicationConnections(packageName string, uid int, count int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired || packageName == "" || count <= 0 {
		return false
	}
	r.manager.RecordApplicationConnections(r.relayID, packageName, uid, count)
	return true
}

func (s *Engine) awaitMobileReady(ctx context.Context, res *candidateResult) (int64, error) {
	started := time.Now()
	readyCtx, cancel := context.WithTimeout(ctx, s.readyLimit())
	defer cancel()
	readyError := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Match desktop's readiness budget error and telemetry classification;
		// parent cancellation/deadlines retain their context identity above.
		return errors.New("tunnel did not become ready in time")
	}
	done := make(chan error, 1)
	go func() { done <- res.mobileRun.WaitReady(readyCtx) }()
	select {
	case err := <-done:
		if readyCtx.Err() != nil {
			return 0, readyError()
		}
		select {
		case stopped := <-res.runDone:
			if stopped == nil {
				stopped = errors.New("engine stopped during readiness")
			}
			return 0, stopped
		default:
		}
		return time.Since(started).Milliseconds(), err
	case err := <-res.runDone:
		if err == nil {
			err = errors.New("engine stopped during readiness")
		}
		return 0, err
	case err := <-res.transportErr:
		if err == nil {
			err = markWSSTransportError("wss_session", res.frontID, errors.New("WSS session stopped during readiness"))
		}
		return 0, err
	case <-readyCtx.Done():
		return 0, readyError()
	}
}

// StateDetails accompanies the state event atomically. SessionID exists only
// during connecting/connected/disconnecting; relay fields exist only while
// CONNECTED. Recovery retains the session but clears relay fields. No relay
// credentials are exposed here. The legacy RelayLabel remains presentation-only.
type StateDetails struct {
	SessionID  string
	RelayID    string
	RelayName  string
	RelayClass string
	// LocationLabel contains only broker-served city/country, never the operator
	// name or relay ID. Empty means the platform should localize "Unknown location".
	// All display text still requires platform control/bidi sanitization.
	LocationLabel string
	Transport     string
	FrontID       string
}

func (s *Engine) detailsLocked() *StateDetails {
	// Leave desktop JSON untouched. Existing ActiveConnectionInfo remains supported.
	if s.Mobile == nil {
		return nil
	}
	d := &StateDetails{}
	if s.core.status == StatusConnecting || s.core.status == StatusConnected || s.core.status == StatusDisconnecting {
		d.SessionID = s.sessionID
	}
	if s.core.status == StatusConnected && s.conn != nil && s.conn.active != nil {
		a := s.conn.active
		d.RelayID, d.RelayName, d.RelayClass = a.relay.ID, relayName(a.relay), brokerapi.EffectiveNodeClass(a.relay.NodeClass)
		d.LocationLabel = mobileLocationLabel(a.relay)
		d.Transport, d.FrontID = a.accessTransport, a.frontID
	}
	return d
}
func relayName(r brokerapi.RelayDescriptor) string {
	if name := strings.TrimSpace(r.Label); name != "" {
		return name
	}
	return r.ID
}

// InstallID returns the same identity used by discovery, tickets and telemetry.
// Mobile never reads or changes HOME/XDG; callers can report this directly to RN.
func (s *Engine) InstallID() (string, error) {
	if s.Mobile != nil {
		if !clienttelemetry.ValidInstallID(s.Mobile.InstallID) {
			return "", errors.New("invalid mobile install UUID")
		}
		return s.Mobile.InstallID, nil
	}
	return clientID()
}
func (s *Engine) appVersion() string {
	if s.Mobile != nil {
		return s.Mobile.AppVersion
	}
	return client.AppVersion()
}
func (s *Engine) platformVersion() string {
	if s.Mobile != nil {
		return s.Mobile.PlatformVersion
	}
	return ""
}

func (s *Engine) mobileAttributes() map[string]string {
	attrs := make(map[string]string)
	if s.Mobile.Attributes != nil {
		for k, v := range s.Mobile.Attributes() {
			attrs[k] = v
		}
	}
	attrs["app_version"] = s.appVersion()
	attrs["platform"] = string(s.Platform)
	attrs["os"] = string(s.Platform)
	if s.Platform == brokerapi.PlatformAndroid {
		attrs["operating_system"] = "Android (API " + s.platformVersion() + ")"
	} else {
		attrs["operating_system"] = "iOS " + s.platformVersion()
	}
	attrs["engine"] = "connectcore"
	attrs["engine_version"] = strings.TrimSpace(engineVersion)
	return attrs
}

//go:embed VERSION
var engineVersion string

// Polling gets the same complete metadata as the last status event, even while
// teardown is releasing resources before its terminal status write.
func (s *Engine) snapshotDetailsLocked() *StateDetails {
	if s.Mobile == nil {
		return nil
	}
	if s.core.details == nil {
		return &StateDetails{}
	}
	copied := *s.core.details
	return &copied
}

func (r *RunTelemetry) traffic() (sent, received int64, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent, r.received, r.hasTraffic
}

// mobileProbeCadence mirrors the shipping Android/iOS traffic-aware probe
// budget: healthy downlink or a successful probe doubles allowance to 5 min;
// sending without a reply and consecutive failures demand a probe each tick.
// A physical-network epoch kick always probes. It can never establish startup
// readiness, and physical-network responses never count as healthy downlink.
type mobileProbeCadence struct {
	base, allowance, remaining time.Duration
	sent, received             int64
	sampled                    bool
}

func (p *mobileProbeCadence) healthy() {
	p.allowance = min(p.allowance*2, 5*time.Minute)
	p.remaining = p.allowance
}
func (p *mobileProbeCadence) failed() { p.allowance = p.base; p.remaining = 0 }
func (p *mobileProbeCadence) due(elapsed time.Duration, sent, received int64, present bool, failures int, forced bool) bool {
	down := present && p.sampled && received > p.received
	up := present && p.sampled && sent > p.sent
	if present {
		p.sent, p.received, p.sampled = sent, received, true
	}
	if forced {
		return true
	}
	if failures == 0 && down {
		p.healthy()
		return false
	}
	p.remaining -= elapsed
	return failures > 0 || (up && !down) || p.remaining <= 0
}

// Keep location separate from the operator-controlled relay name and ID.
// Unlike desktop geoLabel, mobile retains city-only geo and returns empty
// when geo is absent, allowing the host to localize "Unknown location".
func mobileLocationLabel(r brokerapi.RelayDescriptor) string {
	parts := make([]string, 0, 2)
	for _, part := range []string{r.City, r.Country} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}
