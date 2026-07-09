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

	"openrung/internal/client"
)

// detailMaxBytes caps failure_detail at the broker's per-attribute value length,
// so the classifier never emits a value the broker would reject.
const detailMaxBytes = 256

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
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED:
			return "connection_refused"
		case syscall.ECONNRESET:
			return "connection_reset"
		case syscall.ENETUNREACH, syscall.EHOSTUNREACH:
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
	// (Timeout()==false) is never mislabeled.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if os.IsTimeout(err) {
		return "timeout"
	}

	return "unknown"
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
