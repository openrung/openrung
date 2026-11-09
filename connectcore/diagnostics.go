// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// The diagnostic log is a bounded, in-memory record of what the engine did on
// recent connects and directory fetches, for a host's "send diagnostics"
// action. It is never written to disk or sent anywhere by the engine; a host
// exports it only when the user asks (Diagnostics, DiagnosticsJSON,
// DiagnosticsText). Every export is redacted: relay credentials, access and
// lease tokens, UUIDs and IP addresses other than relay and broker-front
// endpoints are replaced before the text leaves the engine.
const (
	diagnosticEventCapacity  = 256
	diagnosticBrokerCapacity = 64
	diagnosticSecretCapacity = 1024
	diagnosticAllowCapacity  = 1024
	diagnosticLineLimit      = 512
	diagnosticDNSAddrLimit   = 4
	diagnosticSubstCapacity  = 32
	// diagnosticMinSecretLen keeps exact-match redaction away from values too
	// short to be distinguished from ordinary words.
	diagnosticMinSecretLen = 6
)

// DiagnosticReport is a redacted snapshot of the diagnostic log.
type DiagnosticReport struct {
	GeneratedAt     time.Time                 `json:"generated_at"`
	EngineVersion   string                    `json:"engine_version"`
	AppVersion      string                    `json:"app_version,omitempty"`
	Platform        string                    `json:"platform"`
	PlatformVersion string                    `json:"platform_version,omitempty"`
	OS              string                    `json:"os"`
	Mode            string                    `json:"mode"`
	Status          string                    `json:"status"`
	LastError       string                    `json:"last_error,omitempty"`
	BrokerAttempts  []DiagnosticBrokerAttempt `json:"broker_attempts"`
	Events          []DiagnosticEvent         `json:"events"`
}

// DiagnosticEvent is one engine log line: relay attempts, transport fallback,
// failover and terminal outcomes, as the engine reported them to its sink.
type DiagnosticEvent struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// DiagnosticBrokerAttempt is the timeline of one HTTP request to a broker
// front. Every *_ms field is milliseconds after Started; a nil field means the
// phase was never reached, which is what locates a stalled connection.
type DiagnosticBrokerAttempt struct {
	Started time.Time `json:"started"`
	// Request is the method and path, without the query.
	Request string `json:"request"`
	// Front is the broker host; FrontKind names which built-in front it is
	// ("cloudfront", "azure", "cloudflare") or "custom".
	Front     string `json:"front"`
	FrontKind string `json:"front_kind"`
	// AzureSNI is set when the Azure front was asked for with its hostname
	// in the TLS handshake rather than without one.
	AzureSNI bool `json:"azure_sni,omitempty"`

	DNSDoneMS  *int64   `json:"dns_done_ms,omitempty"`
	DNSAddrs   []string `json:"dns_addrs,omitempty"`
	DNSError   string   `json:"dns_error,omitempty"`
	Connects   int      `json:"connects,omitempty"`
	RemoteAddr string   `json:"remote_addr,omitempty"`
	// ConnectDoneMS is the last TCP connect to finish; ConnectError its error.
	ConnectDoneMS *int64 `json:"connect_done_ms,omitempty"`
	ConnectError  string `json:"connect_error,omitempty"`
	TLSDoneMS     *int64 `json:"tls_done_ms,omitempty"`
	TLSVersion    string `json:"tls_version,omitempty"`
	TLSError      string `json:"tls_error,omitempty"`
	// ConnReadyMS is when the request had a usable connection; ConnReused
	// says it came from the pool and skipped DNS, TCP and TLS.
	ConnReadyMS      *int64 `json:"conn_ready_ms,omitempty"`
	ConnReused       bool   `json:"conn_reused,omitempty"`
	RequestWrittenMS *int64 `json:"request_written_ms,omitempty"`
	FirstByteMS      *int64 `json:"first_byte_ms,omitempty"`
	StatusCode       int    `json:"status_code,omitempty"`
	BodyBytes        int64  `json:"body_bytes,omitempty"`
	BodyDoneMS       *int64 `json:"body_done_ms,omitempty"`
	// DoneMS is when the attempt ended: the response body was read to its end
	// or failed, or the request failed before a response.
	DoneMS *int64 `json:"done_ms,omitempty"`
	Error  string `json:"error,omitempty"`
}

// diagnosticLog is the engine's ring buffer. Its zero value is ready to use.
type diagnosticLog struct {
	mu      sync.Mutex
	events  []DiagnosticEvent
	brokers []*brokerTrace
	secrets map[string]struct{}
	secretQ []string
	allowed map[string]struct{}
	allowQ  []string
	// substs replaces the user-facing rendering of a tunnel-core error, which
	// quotes the core's own output, with its telemetry-safe rendering.
	substs []diagnosticSubst
}

type diagnosticSubst struct{ unsafe, safe string }

// telemetrySafer is implemented by errors that quote tunnel-core output
// (client.crashError, and in-process runtimes following the TunnelRuntime
// contract); TelemetrySafe is the rendering without the quote.
type telemetrySafer interface{ TelemetrySafe() string }

type brokerTrace struct {
	started time.Time
	a       DiagnosticBrokerAttempt
}

func (d *diagnosticLog) note(line string) {
	d.noteAt(time.Now(), line)
}

func (d *diagnosticLog) noteAt(at time.Time, line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Substitute before truncating, so a quote cut by the limit cannot escape.
	line = d.substituteLocked(line)
	if len(line) > diagnosticLineLimit {
		line = line[:diagnosticLineLimit] + "…"
	}
	d.events = append(d.events, DiagnosticEvent{Time: at, Message: line})
	if len(d.events) > diagnosticEventCapacity {
		d.events = append(d.events[:0:0], d.events[len(d.events)-diagnosticEventCapacity:]...)
	}
}

// learnTunnelError records the telemetry-safe rendering of a tunnel-core
// error before the error reaches any log line or status, so tunnel output it
// quotes (which can name the destinations the user's traffic reached) is
// replaced wherever the engine formats it.
func (d *diagnosticLog) learnTunnelError(err error) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		safe, ok := e.(telemetrySafer)
		if !ok {
			continue
		}
		subst := diagnosticSubst{unsafe: e.Error(), safe: safe.TelemetrySafe()}
		if subst.unsafe == subst.safe || subst.unsafe == "" {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, have := range d.substs {
			if have == subst {
				return
			}
		}
		d.substs = append(d.substs, subst)
		if len(d.substs) > diagnosticSubstCapacity {
			d.substs = append(d.substs[:0:0], d.substs[len(d.substs)-diagnosticSubstCapacity:]...)
		}
		return
	}
}

func (d *diagnosticLog) substituteLocked(s string) string {
	for _, subst := range d.substs {
		s = strings.ReplaceAll(s, subst.unsafe, subst.safe)
	}
	return s
}

// watchTunnelExit forwards a run's exit report after learning its rendering,
// keeping the TunnelRun.Done contract: at most one value, then closed.
func (d *diagnosticLog) watchTunnelExit(done <-chan error) <-chan error {
	out := make(chan error, 1)
	go func() {
		defer close(out)
		for err := range done {
			d.learnTunnelError(err)
			out <- err
		}
	}()
	return out
}

// learnSecret records a credential value to be removed from every export.
func (d *diagnosticLog) learnSecret(values ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < diagnosticMinSecretLen {
			continue
		}
		if d.secrets == nil {
			d.secrets = make(map[string]struct{})
		}
		if _, ok := d.secrets[v]; ok {
			continue
		}
		d.secrets[v] = struct{}{}
		d.secretQ = append(d.secretQ, v)
		if len(d.secretQ) > diagnosticSecretCapacity {
			delete(d.secrets, d.secretQ[0])
			d.secretQ = d.secretQ[1:]
		}
	}
}

// allowAddress records an IP address that may appear in exports: a relay,
// relay-hub or broker-front endpoint. Every other address is redacted, which
// keeps the client's own addresses out of the report. Hostnames are ignored.
func (d *diagnosticLog) allowAddress(hosts ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range hosts {
		h = strings.Trim(strings.TrimSpace(h), "[]")
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		ip := net.ParseIP(h)
		if ip == nil {
			continue
		}
		key := ip.String()
		if d.allowed == nil {
			d.allowed = make(map[string]struct{})
		}
		if _, ok := d.allowed[key]; ok {
			continue
		}
		d.allowed[key] = struct{}{}
		d.allowQ = append(d.allowQ, key)
		if len(d.allowQ) > diagnosticAllowCapacity {
			delete(d.allowed, d.allowQ[0])
			d.allowQ = d.allowQ[1:]
		}
	}
}

// learnRelays registers a directory's credentials for redaction and its
// endpoints as reportable addresses.
func (d *diagnosticLog) learnRelays(relays []brokerapi.RelayDescriptor) {
	for _, r := range relays {
		d.learnSecret(r.ClientID, r.RealityPublicKey, r.ShortID)
		d.allowAddress(r.PublicHost)
		if r.PunchEndpoint != "" {
			if u, err := url.Parse(r.PunchEndpoint); err == nil {
				d.allowAddress(u.Hostname())
			}
		}
	}
}

func (d *diagnosticLog) beginBroker(req *http.Request) *brokerTrace {
	now := time.Now()
	t := &brokerTrace{started: now, a: DiagnosticBrokerAttempt{
		Started:   now,
		Request:   req.Method + " " + req.URL.Path,
		Front:     req.URL.Host,
		FrontKind: brokerFrontKind(req.URL.Hostname()),
	}}
	if t.a.FrontKind == "azure" {
		t.a.AzureSNI = brokerapi.AzureSNIRequested(req.Context())
	}
	d.allowAddress(req.URL.Hostname())
	d.mu.Lock()
	defer d.mu.Unlock()
	d.brokers = append(d.brokers, t)
	if len(d.brokers) > diagnosticBrokerCapacity {
		d.brokers = append(d.brokers[:0:0], d.brokers[len(d.brokers)-diagnosticBrokerCapacity:]...)
	}
	return t
}

// update applies fn to t's attempt under the log lock, passing the elapsed
// milliseconds since the attempt started.
func (d *diagnosticLog) update(t *brokerTrace, fn func(a *DiagnosticBrokerAttempt, ms *int64)) {
	ms := time.Since(t.started).Milliseconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(&t.a, &ms)
}

func brokerFrontKind(host string) string {
	for kind, front := range map[string]string{
		"cloudfront": brokerapi.CloudFrontBrokerURL,
		"azure":      brokerapi.AzureBrokerURL,
		"cloudflare": brokerapi.DefaultBrokerURL,
	} {
		if u, err := url.Parse(front); err == nil && strings.EqualFold(u.Hostname(), host) {
			return kind
		}
	}
	return "custom"
}

func msPtr(ms *int64) *int64 {
	v := *ms
	return &v
}

// diagnosticTransport records a brokerTrace for every request it carries and
// otherwise passes requests to base unchanged: headers, TLS mode selection
// and connection pooling are base's.
type diagnosticTransport struct {
	base http.RoundTripper
	log  *diagnosticLog
}

func (t *diagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d := t.log
	tr := d.beginBroker(req)
	trace := &httptrace.ClientTrace{
		DNSDone: func(info httptrace.DNSDoneInfo) {
			addrs := make([]string, 0, diagnosticDNSAddrLimit)
			for i, a := range info.Addrs {
				if i == diagnosticDNSAddrLimit {
					break
				}
				addrs = append(addrs, a.IP.String())
			}
			// Broker-front answers are reported as they came back: a wrong
			// answer is itself the diagnosis.
			d.allowAddress(addrs...)
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
				a.DNSDoneMS, a.DNSAddrs = msPtr(ms), addrs
				if info.Err != nil {
					a.DNSError = info.Err.Error()
				}
			})
		},
		ConnectStart: func(_, addr string) {
			d.allowAddress(addr)
			d.update(tr, func(a *DiagnosticBrokerAttempt, _ *int64) {
				a.Connects++
				a.RemoteAddr = addr
			})
		},
		ConnectDone: func(_, addr string, err error) {
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
				a.ConnectDoneMS, a.RemoteAddr, a.ConnectError = msPtr(ms), addr, ""
				if err != nil {
					a.ConnectError = err.Error()
				}
			})
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
				if err != nil {
					a.TLSError = err.Error()
					return
				}
				a.TLSDoneMS, a.TLSVersion = msPtr(ms), tls.VersionName(state.Version)
			})
		},
		GotConn: func(info httptrace.GotConnInfo) {
			remote := ""
			if info.Conn != nil && info.Conn.RemoteAddr() != nil {
				remote = info.Conn.RemoteAddr().String()
				d.allowAddress(remote)
			}
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
				a.ConnReadyMS, a.ConnReused = msPtr(ms), info.Reused
				if remote != "" {
					a.RemoteAddr = remote
				}
			})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
				if info.Err != nil {
					a.Error = "write request: " + info.Err.Error()
					return
				}
				a.RequestWrittenMS = msPtr(ms)
			})
		},
		GotFirstResponseByte: func() {
			d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) { a.FirstByteMS = msPtr(ms) })
		},
	}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		https := req.URL.Scheme == "https"
		d.update(tr, func(a *DiagnosticBrokerAttempt, ms *int64) {
			a.DoneMS, a.Error = msPtr(ms), err.Error()
			// A transport with a custom TLS dialer (brokerapi's) handshakes
			// inside the dial, and net/http reports no handshake failure from
			// it. The only step between a completed TCP connect and a usable
			// connection there is the handshake, so the failure is its.
			if https && a.ConnectDoneMS != nil && a.ConnectError == "" &&
				a.ConnReadyMS == nil && a.TLSDoneMS == nil && a.TLSError == "" {
				a.TLSError = err.Error()
			}
		})
		return resp, err
	}
	d.update(tr, func(a *DiagnosticBrokerAttempt, _ *int64) { a.StatusCode = resp.StatusCode })
	resp.Body = &diagnosticBody{ReadCloser: resp.Body, log: d, trace: tr}
	return resp, nil
}

// CloseIdleConnections forwards to base so brokerapi.Client.CloseIdleConnections
// keeps releasing the shared pool.
func (t *diagnosticTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type diagnosticBody struct {
	io.ReadCloser
	log   *diagnosticLog
	trace *brokerTrace
	// done is set once the attempt's end is recorded. Atomic because
	// net/http permits Close concurrently with Read to abort a body.
	done atomic.Bool
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.done.Load() && (n > 0 || err != nil) {
		b.log.update(b.trace, func(a *DiagnosticBrokerAttempt, ms *int64) {
			a.BodyBytes += int64(n)
			if err == nil {
				return
			}
			a.DoneMS = msPtr(ms)
			if err == io.EOF {
				a.BodyDoneMS = msPtr(ms)
			} else {
				a.Error = "read body: " + err.Error()
			}
		})
	}
	if err != nil {
		b.done.Store(true)
	}
	return n, err
}

func (b *diagnosticBody) Close() error {
	if b.done.CompareAndSwap(false, true) {
		b.log.update(b.trace, func(a *DiagnosticBrokerAttempt, ms *int64) {
			if a.DoneMS == nil {
				a.DoneMS = msPtr(ms)
			}
		})
	}
	return b.ReadCloser.Close()
}

// tracedBrokerClient is the engine's broker client (brokerHTTPClient, or
// brokerapi's shared default when that is nil) with each request recorded in
// the diagnostic log. The underlying transport, and so every byte on the
// wire, is the same one the untraced client uses.
func (s *Engine) tracedBrokerClient() *http.Client {
	base := s.brokerHTTPClient()
	if base == nil {
		base = brokerapi.NewHTTPClient(0)
	}
	traced := *base
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	traced.Transport = &diagnosticTransport{base: transport, log: &s.diagnostics}
	return &traced
}

// Diagnostics returns a redacted snapshot of the engine's recent broker
// requests and connection events, oldest first.
func (s *Engine) Diagnostics() DiagnosticReport {
	state := s.State()
	report := DiagnosticReport{
		GeneratedAt:     time.Now().UTC(),
		EngineVersion:   strings.TrimSpace(engineVersion),
		AppVersion:      s.appVersion(),
		Platform:        string(s.telemetryPlatform()),
		PlatformVersion: s.platformVersion(),
		OS:              runtime.GOOS + "/" + runtime.GOARCH,
		Mode:            s.Mode().String(),
		Status:          string(state.Status),
	}
	if state.LastError != nil {
		report.LastError = *state.LastError
	}

	d := &s.diagnostics
	d.mu.Lock()
	report.Events = append([]DiagnosticEvent{}, d.events...)
	report.BrokerAttempts = make([]DiagnosticBrokerAttempt, 0, len(d.brokers))
	for _, t := range d.brokers {
		a := t.a
		a.DNSAddrs = append([]string(nil), a.DNSAddrs...)
		report.BrokerAttempts = append(report.BrokerAttempts, a)
	}
	report.LastError = d.substituteLocked(report.LastError)
	r := newRedactor(d.secrets, d.allowed)
	d.mu.Unlock()

	report.LastError = r.redact(report.LastError)
	for i := range report.Events {
		report.Events[i].Message = r.redact(report.Events[i].Message)
	}
	for i := range report.BrokerAttempts {
		a := &report.BrokerAttempts[i]
		a.Front = r.redact(a.Front)
		a.RemoteAddr = r.redact(a.RemoteAddr)
		a.DNSError = r.redact(a.DNSError)
		a.ConnectError = r.redact(a.ConnectError)
		a.TLSError = r.redact(a.TLSError)
		a.Error = r.redact(a.Error)
		for j := range a.DNSAddrs {
			a.DNSAddrs[j] = r.redact(a.DNSAddrs[j])
		}
	}
	return report
}

// DiagnosticsJSON returns Diagnostics as indented JSON, for hosts (such as
// gomobile bindings) that pass strings across their boundary.
func (s *Engine) DiagnosticsJSON() string {
	out, err := json.MarshalIndent(s.Diagnostics(), "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(out)
}

// DiagnosticsText returns Diagnostics as plain text suitable for an email
// body or a shared file.
func (s *Engine) DiagnosticsText() string {
	return s.Diagnostics().Text()
}

// Text renders the report as plain text.
func (r DiagnosticReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenRung diagnostics\n")
	fmt.Fprintf(&b, "generated: %s\n", diagTime(r.GeneratedAt))
	fmt.Fprintf(&b, "engine: %s  app: %s  platform: %s %s  os: %s  mode: %s\n",
		r.EngineVersion, r.AppVersion, r.Platform, r.PlatformVersion, r.OS, r.Mode)
	fmt.Fprintf(&b, "status: %s\n", r.Status)
	if r.LastError != "" {
		fmt.Fprintf(&b, "last error: %s\n", r.LastError)
	}

	fmt.Fprintf(&b, "\nbroker requests (%d, oldest first; ms after start, - = not reached):\n", len(r.BrokerAttempts))
	for _, a := range r.BrokerAttempts {
		fmt.Fprintf(&b, "%s %s %s (%s", diagTime(a.Started), a.Request, a.Front, a.FrontKind)
		if a.AzureSNI {
			b.WriteString(", sni")
		}
		b.WriteString(")")
		if a.RemoteAddr != "" {
			fmt.Fprintf(&b, " remote=%s", a.RemoteAddr)
		}
		if a.ConnReused {
			b.WriteString(" reused")
		}
		fmt.Fprintf(&b, "\n    dns=%s connect=%s tls=%s conn=%s written=%s first_byte=%s body_done=%s done=%s",
			diagMS(a.DNSDoneMS), diagMS(a.ConnectDoneMS), diagMS(a.TLSDoneMS), diagMS(a.ConnReadyMS),
			diagMS(a.RequestWrittenMS), diagMS(a.FirstByteMS), diagMS(a.BodyDoneMS), diagMS(a.DoneMS))
		if a.Connects > 1 {
			fmt.Fprintf(&b, " connects=%d", a.Connects)
		}
		if a.TLSVersion != "" {
			fmt.Fprintf(&b, " tls_version=%s", a.TLSVersion)
		}
		if a.StatusCode != 0 {
			fmt.Fprintf(&b, " status=%d bytes=%d", a.StatusCode, a.BodyBytes)
		}
		if len(a.DNSAddrs) > 0 {
			fmt.Fprintf(&b, " dns_addrs=%s", strings.Join(a.DNSAddrs, ","))
		}
		for _, e := range []struct{ name, value string }{
			{"dns_error", a.DNSError}, {"connect_error", a.ConnectError},
			{"tls_error", a.TLSError}, {"error", a.Error},
		} {
			if e.value != "" {
				fmt.Fprintf(&b, "\n    %s: %s", e.name, e.value)
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\nengine events (%d, oldest first):\n", len(r.Events))
	for _, e := range r.Events {
		fmt.Fprintf(&b, "%s %s\n", diagTime(e.Time), e.Message)
	}
	return b.String()
}

func diagTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func diagMS(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return fmt.Sprint(*ms)
}

// redactor removes credentials and non-endpoint IP addresses from text.
type redactor struct {
	secrets []string // longest first, so a secret containing another goes whole
	allowed map[string]struct{}
}

func newRedactor(secrets, allowed map[string]struct{}) redactor {
	r := redactor{allowed: make(map[string]struct{}, len(allowed))}
	for s := range secrets {
		r.secrets = append(r.secrets, s)
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	for a := range allowed {
		r.allowed[a] = struct{}{}
	}
	return r
}

var (
	diagJWTRe  = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	diagUUIDRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// diagB64Re matches base64/base64url runs; only mixed-case runs with a
	// digit are replaced (see redact), which spares words and paths.
	diagB64Re = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}={0,2}`)
	// diagHexRe matches standalone hex runs; relay IDs ("relay_" + hex) do not
	// start at a word boundary and are kept.
	diagHexRe = regexp.MustCompile(`\b[0-9A-Fa-f]{8,}\b`)
	// diagIPRe matches candidate IPv4/IPv6 literals, optionally with a port.
	diagIPRe = regexp.MustCompile(`[0-9A-Fa-f:.]*[:.][0-9A-Fa-f:.]*`)
)

func (r redactor) redact(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range r.secrets {
		s = replaceWord(s, secret, "[redacted]")
	}
	s = diagJWTRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && hasUpperAndLower(m) {
			return "[token]"
		}
		return m
	})
	s = diagUUIDRe.ReplaceAllString(s, "[uuid]")
	s = diagB64Re.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && hasUpperAndLower(m) {
			return "[token]"
		}
		return m
	})
	s = replaceHex(s)
	return diagIPRe.ReplaceAllStringFunc(s, r.redactIP)
}

func (r redactor) redactIP(m string) string {
	// Trailing '.' and ':' may be sentence punctuation or IPv6 compression
	// ("2001:db8::"): try the longest prefix first, then drop them one by one.
	for end := len(m); end > 0; end-- {
		if end < len(m) && m[end] != '.' && m[end] != ':' {
			break
		}
		core, suffix := m[:end], m[end:]
		ip, port := net.ParseIP(core), ""
		if ip == nil && strings.Contains(core, ".") {
			// IPv4 with a port.
			if i := strings.LastIndexByte(core, ':'); i > 0 {
				ip, port = net.ParseIP(core[:i]), core[i:]
			}
		}
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			return m
		}
		if _, ok := r.allowed[ip.String()]; ok {
			return m
		}
		return "[ip]" + port + suffix
	}
	return m
}

// replaceHex replaces standalone hex runs that mix digits and letters, the
// shape of short IDs and hex keys. A run that is a DNS label (adjacent to a
// dot and another label) is a hostname, which is kept.
func replaceHex(s string) string {
	matches := diagHexRe.FindAllStringIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		run := s[m[0]:m[1]]
		label := (m[1]+1 < len(s) && s[m[1]] == '.' && isAlnum(s[m[1]+1])) ||
			(m[0] >= 2 && s[m[0]-1] == '.' && isAlnum(s[m[0]-2]))
		if label || !strings.ContainsAny(run, "0123456789") || !strings.ContainsAny(strings.ToLower(run), "abcdef") {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("[hex]")
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func hasUpperAndLower(s string) bool {
	return strings.ContainsFunc(s, func(c rune) bool { return c >= 'A' && c <= 'Z' }) &&
		strings.ContainsFunc(s, func(c rune) bool { return c >= 'a' && c <= 'z' })
}

// replaceWord replaces occurrences of old in s that are not part of a longer
// alphanumeric run, so a short credential cannot clip an ordinary word.
func replaceWord(s, old, replacement string) string {
	if old == "" || !strings.Contains(s, old) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(old)
		if (i > 0 && isAlnum(s[i-1])) || (end < len(s) && isAlnum(s[end])) {
			b.WriteString(s[:i+1])
			s = s[i+1:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteString(replacement)
		s = s[end:]
	}
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// noteDiagnostic records an engine event in the diagnostic log only, without
// sending it to the host's sink.
func (s *Engine) noteDiagnostic(format string, args ...any) {
	s.diagnostics.note(fmt.Sprintf(format, args...))
}
