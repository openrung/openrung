package wsscore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
)

// The closed WSS dial-failure taxonomy. These tokens are the ONLY failure
// information that leaves DialClient: the underlying error chain, the CDN
// response status, headers, and body are classified here and then destroyed.
// The set is frozen — every token is also a censor-writable signal (the censor
// chooses whether to refuse, drop, reset, or answer with a status), so adding
// a token requires the privacy review described in the README token registry.
const (
	// HTTP responses from the CDN edge or the relay sidecar (the censored path
	// worked end to end well enough to carry an HTTP response back).
	ReasonWSUpgrade   = "ws_upgrade"
	ReasonHTTP401     = "http_401"
	ReasonHTTP403     = "http_403"
	ReasonHTTP421     = "http_421"
	ReasonRateLimited = "rate_limited"
	ReasonHTTP502     = "http_502"
	ReasonHTTP503     = "http_503"
	ReasonHTTPOther   = "http_other"

	// Post-101 WebSocket negotiation failures detected by DialClient itself.
	ReasonWSSubprotocol = "ws_subprotocol"

	// Name resolution.
	ReasonDNSBogon   = "dns_bogon"
	ReasonDNSFailure = "dns_failure"

	// Local intent.
	ReasonCancelled = "cancelled"

	// Socket-level outcomes, split by handshake phase where that changes the
	// interpretation.
	ReasonConnectionRefused  = "connection_refused"
	ReasonNetworkUnreachable = "network_unreachable"
	ReasonConnectionReset    = "connection_reset"
	ReasonTLSReset           = "tls_reset"
	ReasonResponseReset      = "response_reset"

	// TLS handshake outcomes.
	ReasonTLSNotTLS    = "tls_not_tls"
	ReasonCertExpired  = "cert_expired"
	ReasonCertVerify   = "cert_verify"
	ReasonTLSAlert     = "tls_alert"
	ReasonTLSHandshake = "tls_handshake"

	// Timeouts, split by handshake phase.
	ReasonTCPTimeout       = "tcp_timeout"
	ReasonTLSTimeout       = "tls_timeout"
	ReasonResponseTimeout  = "response_timeout"
	ReasonHandshakeTimeout = "handshake_timeout"

	// Nothing matched. Deliberately distinct from the legacy "unknown" so the
	// share of unclassified events measures this taxonomy's own coverage.
	ReasonUnclassified = "unclassified"
)

// Reasons returns every token of the closed taxonomy above, in declaration
// order. It is the machine-readable form of the README's token registry:
// consumers that project these tokens (openrung/internal/clienttelemetry's
// allowlist) walk it in tests, so a token added to the const block without a
// decision at every consumer fails CI instead of shipping unexamined.
// TestReasonsCoversTheConstBlock keeps this list in lockstep with the
// constants.
func Reasons() []string {
	return []string{
		ReasonWSUpgrade, ReasonHTTP401, ReasonHTTP403, ReasonHTTP421,
		ReasonRateLimited, ReasonHTTP502, ReasonHTTP503, ReasonHTTPOther,
		ReasonWSSubprotocol,
		ReasonDNSBogon, ReasonDNSFailure,
		ReasonCancelled,
		ReasonConnectionRefused, ReasonNetworkUnreachable,
		ReasonConnectionReset, ReasonTLSReset, ReasonResponseReset,
		ReasonTLSNotTLS, ReasonCertExpired, ReasonCertVerify,
		ReasonTLSAlert, ReasonTLSHandshake,
		ReasonTCPTimeout, ReasonTLSTimeout,
		ReasonResponseTimeout, ReasonHandshakeTimeout,
		ReasonUnclassified,
	}
}

// DialError is the only error DialClient returns for a failed WSS handshake
// (besides the bare ErrSocketProtectionFailed sentinel). Its fields are
// unexported and it deliberately has no Unwrap: the underlying error chain
// embeds the CDN hostname, resolved edge addresses, and certificate subjects,
// none of which may cross this module's API, the gomobile boundary, logs, or
// telemetry. Error always returns a compile-time fixed string.
type DialError struct {
	reason  string
	message string
}

func (e *DialError) Error() string { return e.message }

// Reason returns the closed classification token. Consumers must never
// serialize anything but this token.
func (e *DialError) Reason() string { return e.reason }

// Is keeps errors.Is(err, context.Canceled) working for callers that test for
// local cancellation. No other target matches.
func (e *DialError) Is(target error) bool {
	return e.reason == ReasonCancelled && target == context.Canceled
}

// FailureReason returns the classification token carried by err, or "" when
// the chain holds no DialError (including the bare ErrSocketProtectionFailed
// sentinel, which callers already match with errors.Is).
func FailureReason(err error) string {
	var dialErr *DialError
	if errors.As(err, &dialErr) {
		return dialErr.reason
	}
	return ""
}

// SocketErrnoReason returns the token classifyDialFailure assigns to a bare
// socket errno at the earliest dial phase (no TCP connect, no TLS started),
// which is where a raw errno arrives; the phase-split variants (tls_reset,
// response_timeout, …) exist only inside a dial that progressed further.
// ReasonUnclassified for an errno this taxonomy deliberately does not map.
// Exported so a consumer that keeps its own errno→token mapping over the
// shared Winsock table (winsock.go) can machine-check where the two taxonomies
// agree and pin where they deliberately diverge, instead of asserting it in
// comments.
func SocketErrnoReason(errno syscall.Errno) string {
	return classifyDialFailure(context.Background(), errno, nil, &dialPhases{})
}

func newDialError(reason string) *DialError {
	return &DialError{reason: reason, message: "WSS handshake failed (" + reason + ")"}
}

// newDialErrorWithMessage exists for the two post-101 checks whose exact
// pre-taxonomy message strings are pinned by tests. message must be a
// compile-time fixed string, never derived from an underlying error.
func newDialErrorWithMessage(reason, message string) *DialError {
	return &DialError{reason: reason, message: message}
}

// dialPhases records how far one dial progressed, as booleans only — never
// timestamps or durations, which would open a censor-writable timing channel.
// Stores are atomic because the network dial, the TLS handshake, and httptrace
// callbacks can run on other goroutines than the classifier.
type dialPhases struct {
	tcpConnected         atomic.Bool
	tlsStarted           atomic.Bool
	tlsDone              atomic.Bool
	gotFirstResponseByte atomic.Bool
	bogonAddress         atomic.Bool
}

// bogonPrefixes lists the reserved ranges without a dedicated netip helper.
// Together with the helper checks in isBogonAddress the flagged set is exactly:
// RFC 1918 (10/8, 172.16/12, 192.168/16), 127/8, 169.254/16, CGNAT 100.64/10,
// 0.0.0.0/8, 240/4, ::1, fc00::/7, and fe80::/10.
var bogonPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// isBogonAddress reports whether a resolved dial target sits in reserved or
// special-purpose address space. A public CDN hostname must never resolve
// there, so a match is a definitive DNS-injection signature (Iranian injection
// classically answers with 10.10.34.x). Only the boolean outcome survives; the
// address itself is discarded with the rest of the dial state.
func isBogonAddress(address string) bool {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return false
	}
	addr := addrPort.Addr().Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return true
	}
	for _, prefix := range bogonPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// bogonAwareControl observes the per-attempt resolved address, sets the bogon
// flag, immediately discards the address, and then delegates to the protector
// control unchanged — it never vetoes or alters the dial itself, and the
// delegate's sentinel, fail-closed, and panic semantics are preserved.
func bogonAwareControl(
	phases *dialPhases,
	delegate func(context.Context, string, string, syscall.RawConn) error,
) func(context.Context, string, string, syscall.RawConn) error {
	return func(ctx context.Context, network, address string, raw syscall.RawConn) error {
		if phases != nil && isBogonAddress(address) {
			phases.bogonAddress.Store(true)
		}
		if delegate == nil {
			return nil
		}
		return delegate(ctx, network, address, raw)
	}
}

// classifyDialFailure maps a failed dial to one closed token while the typed
// error chain, the CDN response status, and the phase booleans still exist.
// It must run before resp.Body is closed and must be the last reader of all
// three inputs. The dial context is deliberately not consulted: attribution is
// error-first, so a real network failure racing a concurrent cancellation
// keeps its causal token instead of collapsing to "cancelled".
func classifyDialFailure(_ context.Context, err error, resp *http.Response, phases *dialPhases) string {
	// An HTTP response proves the censored network path carried bytes both
	// ways; the status alone (read before the body is destroyed) names whether
	// the CDN edge, the sidecar, or upgrade negotiation refused.
	if resp != nil {
		return classifyHTTPStatus(resp.StatusCode)
	}
	// A dial directed into reserved address space is a poisoned DNS answer;
	// that root cause outranks whatever the doomed connection then reported.
	if phases.bogonAddress.Load() {
		return ReasonDNSBogon
	}
	if errors.Is(err, context.Canceled) {
		return ReasonCancelled
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ReasonDNSFailure
	}
	// Each case pairs the portable POSIX name with its Winsock number from the
	// shared table (winsock.go), matched unconditionally: Go defines the E*
	// names on Windows as invented values while the net package surfaces the
	// raw Winsock numbers there, the two sets cannot collide, and matching
	// both everywhere keeps the Windows mappings testable on any CI host.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, WSAECONNREFUSED:
			return ReasonConnectionRefused
		case syscall.ENETUNREACH, syscall.EHOSTUNREACH, WSAENETUNREACH, WSAEHOSTUNREACH:
			return ReasonNetworkUnreachable
		case syscall.ECONNRESET, WSAECONNRESET:
			return resetReason(phases)
		}
	}
	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) {
		return ReasonTLSNotTLS
	}
	if reason, ok := certificateReason(err); ok {
		return reason
	}
	// crypto/tls surfaces a fatal alert from the peer as a net.OpError with
	// this fixed Op; there is no exported alert error type to match instead.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" {
		return ReasonTLSAlert
	}
	// The raw WSAETIMEDOUT needs naming: Windows syscall.Errno.Timeout only
	// reports true for the invented EAGAIN/EWOULDBLOCK/ETIMEDOUT, so the
	// generic timeout rule misses it (POSIX ETIMEDOUT reports true and needs
	// no explicit case).
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errno == WSAETIMEDOUT {
		return timeoutReason(phases)
	}
	// WSAECONNABORTED, the local stack aborting a connection, is the closest
	// analogue of a broken pipe: it is what a send raises after the connection
	// has gone away without a peer reset.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errno == syscall.EPIPE || errno == WSAECONNABORTED {
		return resetReason(phases)
	}
	if phases.tlsStarted.Load() && !phases.tlsDone.Load() {
		return ReasonTLSHandshake
	}
	return ReasonUnclassified
}

func classifyHTTPStatus(status int) string {
	switch status {
	case http.StatusSwitchingProtocols:
		// gorilla returns the 101 response when its Upgrade, Connection, or
		// Sec-WebSocket-Accept validation failed.
		return ReasonWSUpgrade
	case http.StatusUnauthorized:
		return ReasonHTTP401
	case http.StatusForbidden:
		return ReasonHTTP403
	case http.StatusMisdirectedRequest:
		return ReasonHTTP421
	case http.StatusTooManyRequests:
		return ReasonRateLimited
	case http.StatusBadGateway:
		return ReasonHTTP502
	case http.StatusServiceUnavailable:
		return ReasonHTTP503
	default:
		// Never interpolate a raw status beyond the fixed list above.
		return ReasonHTTPOther
	}
}

// certificateReason matches every certificate-verification failure shape the
// no-SNI hook and crypto/tls's built-in path produce. An expired-certificate
// chain is split out because it overwhelmingly means a wrong device clock;
// everything else stays in the high-precision active-interception token.
func certificateReason(err error) (string, bool) {
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		if invalidErr.Reason == x509.Expired {
			return ReasonCertExpired, true
		}
		return ReasonCertVerify, true
	}
	var (
		verificationErr *tls.CertificateVerificationError
		unknownAuthErr  x509.UnknownAuthorityError
		hostnameErr     x509.HostnameError
	)
	if errors.As(err, &verificationErr) ||
		errors.As(err, &unknownAuthErr) ||
		errors.As(err, &hostnameErr) {
		return ReasonCertVerify, true
	}
	return "", false
}

func resetReason(phases *dialPhases) string {
	switch {
	case phases.tlsStarted.Load() && !phases.tlsDone.Load():
		return ReasonTLSReset
	case phases.tlsDone.Load():
		return ReasonResponseReset
	default:
		return ReasonConnectionReset
	}
}

func timeoutReason(phases *dialPhases) string {
	switch {
	case !phases.tcpConnected.Load():
		return ReasonTCPTimeout
	case phases.tlsStarted.Load() && !phases.tlsDone.Load():
		return ReasonTLSTimeout
	case phases.tlsDone.Load() && !phases.gotFirstResponseByte.Load():
		return ReasonResponseTimeout
	default:
		return ReasonHandshakeTimeout
	}
}
