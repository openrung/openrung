package clienttelemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"openrung/internal/client"
)

// statusStub is an error carrying an HTTP status, mirroring what the broker
// fetch paths return (BrokerStatusError / RateLimitedError).
type statusStub struct{ code int }

func (s statusStub) Error() string   { return fmt.Sprintf("broker status %d", s.code) }
func (s statusStub) HTTPStatus() int { return s.code }

// timeoutStub is a net.Error reporting a timeout without any underlying errno.
type timeoutStub struct{}

func (timeoutStub) Error() string   { return "i/o timeout" }
func (timeoutStub) Timeout() bool   { return true }
func (timeoutStub) Temporary() bool { return false }

func exitError(t *testing.T) *exec.ExitError {
	t.Helper()
	// `false` exits non-zero, yielding an *exec.ExitError from Run.
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Skipf("could not produce *exec.ExitError: %v", err)
	}
	return exitErr
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"cancelled", context.Canceled, "cancelled"},
		{"cancelled_wrapped", fmt.Errorf("connect: %w", context.Canceled), "cancelled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"no_relays_available", client.ErrNoRelaysAvailable, "no_relays_available"},
		{"relay_not_in_list", fmt.Errorf("relay %q: %w", "x", client.ErrRelayNotInList), "relay_not_in_list"},
		{"no_relay_in_country", fmt.Errorf("country US: %w", client.ErrNoRelayInCountry), "no_relay_in_country"},
		{"no_usable_relay", client.ErrNoUsableRelay, "no_usable_relay"},
		{"rate_limited", statusStub{code: 429}, "rate_limited"},
		{"http_503", statusStub{code: 503}, "http_503"},
		{"http_wrapped", fmt.Errorf("fetch: %w", statusStub{code: 500}), "http_500"},
		{"connection_refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "connection_refused"},
		{"connection_reset", fmt.Errorf("read: %w", syscall.ECONNRESET), "connection_reset"},
		{"network_unreachable_net", fmt.Errorf("dial: %w", syscall.ENETUNREACH), "network_unreachable"},
		{"network_unreachable_host", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), "network_unreachable"},
		{
			"connection_refused_syscallerror",
			&os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
			"connection_refused",
		},
		{
			"connection_refused_opError",
			&net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			"connection_refused",
		},
		{"dns_failure", &net.DNSError{Err: "no such host", Name: "broker"}, "dns_failure"},
		{"dns_failure_urlwrapped", &url.Error{Op: "Get", URL: "https://b", Err: &net.DNSError{Err: "nxdomain"}}, "dns_failure"},
		{"tls_record_header", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "tls_handshake"},
		{"tls_unknown_authority", x509.UnknownAuthorityError{}, "tls_handshake"},
		{"tls_hostname", x509.HostnameError{Host: "broker"}, "tls_handshake"},
		{"tls_cert_invalid", x509.CertificateInvalidError{Reason: x509.Expired}, "tls_handshake"},
		{"permission_denied", os.ErrPermission, "permission_denied"},
		{"permission_denied_eacces", fmt.Errorf("open: %w", syscall.EACCES), "permission_denied"},
		{"timeout_net", timeoutStub{}, "timeout"},
		{"timeout_net_wrapped", fmt.Errorf("dial: %w", timeoutStub{}), "timeout"},
		{"unknown", errors.New("something odd"), "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}

	t.Run("process_exited", func(t *testing.T) {
		if got := ClassifyError(fmt.Errorf("sing-box exited: %w", exitError(t))); got != "process_exited" {
			t.Fatalf("process_exited: got %q", got)
		}
	})
}

func TestErrorDetail(t *testing.T) {
	if got := ErrorDetail(nil); got != "" {
		t.Fatalf("nil detail = %q, want empty", got)
	}

	short := errors.New("boom")
	if got := ErrorDetail(short); got != "boom" {
		t.Fatalf("short detail = %q", got)
	}

	// Exactly at the cap: preserved verbatim.
	exact := errors.New(strings.Repeat("a", detailMaxBytes))
	if got := ErrorDetail(exact); len(got) != detailMaxBytes || got != strings.Repeat("a", detailMaxBytes) {
		t.Fatalf("exact detail len = %d, want %d", len(got), detailMaxBytes)
	}

	// Over the cap (ASCII): truncated to the cap.
	long := errors.New(strings.Repeat("b", detailMaxBytes+50))
	if got := ErrorDetail(long); len(got) != detailMaxBytes {
		t.Fatalf("long detail len = %d, want %d", len(got), detailMaxBytes)
	}

	// Multi-byte runes straddling the cap: never split a rune.
	multi := errors.New(strings.Repeat("界", detailMaxBytes)) // 3 bytes each
	got := ErrorDetail(multi)
	if len(got) > detailMaxBytes {
		t.Fatalf("multibyte detail len = %d, exceeds cap %d", len(got), detailMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("multibyte detail is not valid UTF-8: %q", got)
	}
	// The cap (256) is not a multiple of 3, so a naive cut would leave a partial
	// rune; the trimmed result must be the largest whole-rune prefix that fits.
	if want := strings.Repeat("界", detailMaxBytes/3); got != want {
		t.Fatalf("multibyte detail = %q, want %q", got, want)
	}
}
