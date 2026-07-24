// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
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
}

func (e *RelayListVerificationError) Error() string {
	return "unsigned/invalid relay list: " + e.Reason
}

func verificationFailure(format string, args ...any) error {
	return &RelayListVerificationError{Reason: fmt.Sprintf(format, args...)}
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
