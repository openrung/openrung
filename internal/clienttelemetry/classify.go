package clienttelemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"unicode/utf8"

	"github.com/openrung/openrung/wsscore"

	"openrung/internal/client"
)

// detailMaxBytes caps failure_detail at the broker's per-attribute value length,
// so the classifier never emits a value the broker would reject.
const detailMaxBytes = 256

// Winsock error numbers, matched alongside the syscall.E* names below.
//
// Go's syscall package defines ECONNREFUSED and friends on Windows as invented
// APPLICATION_ERROR values (1<<29 and up), while the net package surfaces the
// raw Winsock numbers from connectex/WSARecv untranslated. Matching only the E*
// names therefore compiles cleanly on Windows and never fires, so every Windows
// socket failure degraded to "unknown". wsscore solves this with a build-tagged
// constant set (wsscore/failure_posix.go, wsscore/failure_windows.go); this
// classifier maps the same conditions to the same tokens.
//
// The numbers are matched unconditionally rather than behind a build tag: they
// sit far above every errno any POSIX kernel returns (Linux tops out at 133,
// darwin at 106), so they cannot collide with a real POSIX errno, and matching
// them on every platform is what lets the cross-language contract vectors
// exercise the Windows rows in CI on Linux and macOS. They are spelled as
// literals because syscall exports only WSAECONNRESET by name, and only on
// Windows.
const (
	wsaeNetUnreach  = syscall.Errno(10051) // WSAENETUNREACH
	wsaeConnReset   = syscall.Errno(10054) // WSAECONNRESET
	wsaeTimedOut    = syscall.Errno(10060) // WSAETIMEDOUT
	wsaeConnRefused = syscall.Errno(10061) // WSAECONNREFUSED
	wsaeHostUnreach = syscall.Errno(10065) // WSAEHOSTUNREACH
)

// httpStatusError is implemented by broker errors that carry an HTTP status
// code (internal/client.BrokerStatusError, discovery.RateLimitedError). Matching
// on this interface keeps the classifier free of any fetch-package import.
type httpStatusError interface {
	HTTPStatus() int
}

// ClassifyError maps err to a stable lowercase snake_case failure_reason token.
// It inspects the whole error chain with typed checks (errors.Is/errors.As); the
// returned tokens are a fixed enum the iOS/Android clients must mirror. "" for a
// nil error; "unknown" when nothing matches.
//
// A failed WSS handshake carries wsscore's own closed taxonomy (ws_upgrade,
// http_403, tls_timeout, dns_bogon, …); wsscore classified it at the only point
// where the typed error and the CDN status still existed, so that token is
// authoritative here. See wsscore/README.md for the token registry.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}

	// Context outcomes take precedence: a cancelled or timed-out connect can wrap
	// any lower-level error, and the intent is what the dashboard wants to see.
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}

	// The WSS transport pre-classified this failure. Its token is more specific
	// than every generic rule below, which would only ever see the deliberately
	// information-free DialError and return "unknown".
	var dialErr *wsscore.DialError
	if errors.As(err, &dialErr) {
		return wssFailureReason(dialErr.Reason())
	}

	// Relay-selection sentinels (internal/client), distinct so the dashboard can
	// tell "broker gave nothing" from "none usable" from a bad target id/country.
	switch {
	case errors.Is(err, client.ErrNoRelaysAvailable):
		return "no_relays_available"
	case errors.Is(err, client.ErrRelayNotInList):
		return "relay_not_in_list"
	case errors.Is(err, client.ErrNoRelayInCountry):
		return "no_relay_in_country"
	case errors.Is(err, client.ErrNoUsableRelay):
		return "no_usable_relay"
	}

	// Broker HTTP status, via the interface so no fetch package is imported.
	var statusErr httpStatusError
	if errors.As(err, &statusErr) {
		if statusErr.HTTPStatus() == http.StatusTooManyRequests {
			return "rate_limited"
		}
		return fmt.Sprintf("http_%d", statusErr.HTTPStatus())
	}

	// Syscall-level network errors (dial/read failures wrap a syscall.Errno).
	// Each case pairs the POSIX errno with its Winsock number; on Windows only
	// the latter ever arrives. WSAECONNABORTED (10053) is deliberately absent:
	// its closest POSIX analogue is EPIPE, which classifies as "unknown" here,
	// and mapping one without the other would make the same broken connection
	// report different tokens per platform.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, wsaeConnRefused:
			return "connection_refused"
		case syscall.ECONNRESET, wsaeConnReset:
			return "connection_reset"
		case syscall.ENETUNREACH, syscall.EHOSTUNREACH, wsaeNetUnreach, wsaeHostUnreach:
			return "network_unreachable"
		}
	}

	// DNS resolution failure, before the generic timeout check so a name-lookup
	// timeout still classifies as dns_failure (the more actionable signal).
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failure"
	}

	// TLS: a plaintext/garbled record header, or any certificate rejection.
	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) {
		return "tls_handshake"
	}
	var (
		unknownAuthErr x509.UnknownAuthorityError
		certInvalidErr x509.CertificateInvalidError
		hostnameErr    x509.HostnameError
	)
	if errors.As(err, &unknownAuthErr) ||
		errors.As(err, &certInvalidErr) ||
		errors.As(err, &hostnameErr) {
		return "tls_handshake"
	}

	// os.ErrPermission also catches EACCES/EPERM via syscall.Errno.Is.
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}

	// sing-box (or any exec'd tunnel) died on arrival.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "process_exited"
	}

	// Generic i/o timeout, after the typed checks above so a refused/reset dial
	// (Timeout()==false) is never mislabeled, and after the DNS check so a
	// name-lookup timeout keeps the more actionable token.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if os.IsTimeout(err) {
		return "timeout"
	}
	// syscall.Errno.Timeout on Windows reports true only for Go's invented
	// EAGAIN/EWOULDBLOCK/ETIMEDOUT, so a real WSAETIMEDOUT reaches neither check
	// above and needs naming here (as it does in wsscore's timeout rule).
	if errno == wsaeTimedOut {
		return "timeout"
	}

	return "unknown"
}

// wssFailureReason projects wsscore's dial-failure token through the explicit
// literal allowlist the taxonomy contract requires of every consumer
// (wsscore/README.md): anything unrecognized degrades to the generic
// "wss_transport_failed" instead of reaching telemetry verbatim. The literals
// are deliberate — referencing the wsscore constants would let a future token
// addition or value change flow into the telemetry channel without a decision
// here, and the frozen set makes every such change a privacy-review event.
// "unknown" stays reserved for pre-taxonomy builds, and "unclassified" for the
// taxonomy's own coverage residual, so the degraded value is neither.
func wssFailureReason(reason string) string {
	switch reason {
	case "ws_upgrade", "http_401", "http_403", "http_421", "rate_limited",
		"http_502", "http_503", "http_other",
		"ws_subprotocol",
		"dns_bogon", "dns_failure",
		"cancelled",
		"connection_refused", "network_unreachable",
		"connection_reset", "tls_reset", "response_reset",
		"tls_not_tls", "cert_expired", "cert_verify", "tls_alert", "tls_handshake",
		"tcp_timeout", "tls_timeout", "response_timeout", "handshake_timeout",
		"unclassified":
		return reason
	}
	return "wss_transport_failed"
}

// ErrorDetail returns err.Error() truncated to at most detailMaxBytes, on a
// UTF-8 rune boundary so the broker never sees an invalid attribute value. "" for
// a nil error.
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= detailMaxBytes {
		return msg
	}
	truncated := msg[:detailMaxBytes]
	// Drop a trailing partial rune left by the byte-length cut.
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}
