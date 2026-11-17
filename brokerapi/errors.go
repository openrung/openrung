// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BrokerStatusError reports a bounded, sanitized broker error response.
type BrokerStatusError struct {
	Operation  string
	BrokerURL  string
	StatusCode int
	Message    string
}

func (e *BrokerStatusError) Error() string {
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = "request"
	}
	if e.Message != "" {
		return fmt.Sprintf("broker %s: %s", operation, e.Message)
	}
	return fmt.Sprintf("broker %s failed with HTTP status %d", operation, e.StatusCode)
}

// HTTPStatus exposes the response status for telemetry classification.
func (e *BrokerStatusError) HTTPStatus() int { return e.StatusCode }

// RateLimitedError retains the serving broker and bounded Retry-After hint.
type RateLimitedError struct {
	BrokerURL  string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("broker %s rate-limited (retry after %s)", e.BrokerURL, e.RetryAfter)
	}
	return fmt.Sprintf("broker %s rate-limited", e.BrokerURL)
}

func (e *RateLimitedError) HTTPStatus() int { return http.StatusTooManyRequests }

// WSSTicketStatusError deliberately omits the response body, ticket, and URL.
type WSSTicketStatusError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *WSSTicketStatusError) Error() string {
	return fmt.Sprintf("broker WSS ticket request failed with HTTP status %d", e.StatusCode)
}

func (e *WSSTicketStatusError) HTTPStatus() int { return e.StatusCode }

// RelayListVerificationError identifies a failed signature, envelope, or
// freshness check. No non-loopback relay JSON is returned on this error.
type RelayListVerificationError struct {
	Reason string
	// Expired marks a response that passed every check except freshness: its
	// signature verified, but not_after is past by the local clock.
	Expired bool
}

func (e *RelayListVerificationError) Error() string {
	return "unsigned/invalid relay list: " + e.Reason
}

// Is lets errors.Is(err, ErrSignedResponseExpired) find an expiry failure.
func (e *RelayListVerificationError) Is(target error) bool {
	return target == ErrSignedResponseExpired && e.Expired
}

// ErrSignedResponseExpired matches a RelayListVerificationError whose only
// failed check was freshness. The response is genuine, so the usual cause is
// a local clock that runs ahead; hosts use it to ask the user to check the
// device's date and time.
var ErrSignedResponseExpired = errors.New("signed broker response expired by the local clock")

// ClockSkewSuspected reports whether err is a failure that a wrong local
// clock produces, in either direction: a genuine signed response that is
// already expired (clock ahead), or a broker certificate outside its validity
// period (clock far ahead or behind). Hosts use it to ask the user to check
// the device's date and time; it is a hint, not proof.
func ClockSkewSuspected(err error) bool {
	if errors.Is(err, ErrSignedResponseExpired) || errors.Is(err, errClockSkewHint) {
		return true
	}
	var invalid x509.CertificateInvalidError
	return errors.As(err, &invalid) && invalid.Reason == x509.Expired
}

// errClockSkewHint is matched by clockSkewHintError only.
var errClockSkewHint = errors.New("another broker candidate failed in a way a wrong clock produces")

// clockSkewHintError is the error discovery reports for a set of failed
// candidates when a candidate other than the reported one failed in a way a
// wrong clock produces. Its text and chain are the reported error's, so
// diagnostics and classification do not change; only ClockSkewSuspected
// sees the difference.
type clockSkewHintError struct{ error }

func (e *clockSkewHintError) Unwrap() error { return e.error }

func (e *clockSkewHintError) Is(target error) bool { return target == errClockSkewHint }

// withClockSkewHint returns primary, marked for ClockSkewSuspected when it is
// not already suspect but one of the other candidates' errors is.
func withClockSkewHint(primary error, others ...error) error {
	if primary == nil || ClockSkewSuspected(primary) {
		return primary
	}
	for _, other := range others {
		if ClockSkewSuspected(other) {
			return &clockSkewHintError{primary}
		}
	}
	return primary
}

func verificationFailure(format string, args ...any) error {
	return &RelayListVerificationError{Reason: fmt.Sprintf(format, args...)}
}

func expiredFailure(format string, args ...any) error {
	return &RelayListVerificationError{Reason: fmt.Sprintf(format, args...), Expired: true}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 || seconds > int64((24*time.Hour)/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	delay := when.Sub(now)
	if delay > 24*time.Hour {
		return 0
	}
	return delay
}
