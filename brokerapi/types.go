// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const (
	cloudflareBrokerHost = "broker.openrung.org"
	cloudFrontBrokerHost = "d2r7mdpyevvs1m.cloudfront.net"

	// DefaultBrokerURL is the Cloudflare broker front. Direct connections to
	// its standard HTTPS port opportunistically use the embedded ECH config.
	DefaultBrokerURL = "https://" + cloudflareBrokerHost + "/"

	// CloudFrontBrokerURL is the independent AWS front. CloudFront cannot
	// receive this deployment's ECH config, so direct connections to it omit
	// SNI instead and let the encrypted HTTP Host header select the
	// distribution.
	CloudFrontBrokerURL = "https://" + cloudFrontBrokerHost + "/"

	DefaultRelayLimit       = 5
	DefaultDiscoveryStagger = 2500 * time.Millisecond
	DefaultRelayListTimeout = 15 * time.Second
	DefaultTelemetryTimeout = 15 * time.Second
	DefaultWSSTicketTimeout = 5 * time.Second
	DefaultHealthTimeout    = 4 * time.Second
	DefaultSpeedTestTimeout = 60 * time.Second
)

// Platform selects the fixed platform-identification header emitted by a
// Client. It deliberately is not an arbitrary header name.
type Platform string

const (
	PlatformNone        Platform = ""
	PlatformDesktop     Platform = "desktop"
	PlatformReactNative Platform = "react-native"
	PlatformAndroid     Platform = "android"
	PlatformIOS         Platform = "ios"
)

// Options is immutable request metadata attached to every request made by a
// Client. PlatformVersion is the platform header value: Android API level,
// iOS version, or a React Native OS token. Desktop and React Native infer a
// token from the Go target when it is blank.
type Options struct {
	AppVersion      string
	Platform        Platform
	PlatformVersion string
}

// Identity is sent only when both fields are present. This preserves the
// broker's paired client/session identity contract and prevents half-identities
// from escaping on the wire.
type Identity struct {
	ClientID  string
	SessionID string
}

// RelayList retains the exact response bytes that were signature-verified and
// schema-validated. Consumers obtain a copy through JSON and decode it into
// their platform-owned relay model only after this package returns.
type RelayList struct {
	rawJSON           []byte
	KeyID             string
	SignatureVerified bool
}

// JSON returns a fresh copy of the exact response bytes that passed signature
// and wire-schema verification.
func (r RelayList) JSON() []byte {
	return bytes.Clone(r.rawJSON)
}

// Candidates is the ordered set used by FirstReachable. OverrideFirst means
// URLs[0] is a genuine user override and must finish before defaults are raced.
type Candidates struct {
	URLs          []string
	OverrideFirst bool
}

// Fetch identifies both the verified relay bytes and the broker front that
// served them, so later control-plane requests can prefer the same front.
type Fetch struct {
	BrokerURL string
	RelayList RelayList
}

// ListOptions controls a relay-list fetch and the staggered discovery race.
type ListOptions struct {
	Limit    int
	Identity Identity
	Stagger  time.Duration
}

// Client makes broker requests with a single URL, header, redirect, signing,
// and TLS policy.
type Client struct {
	httpClient *http.Client
	options    Options
}

// NewClient constructs a shared broker client. A nil HTTP client selects the
// package's ECH-capable transport. A supplied client is shallow-cloned so
// redirect refusal cannot mutate the caller's instance.
func NewClient(httpClient *http.Client, options Options) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient(0)
	} else {
		cloned := *httpClient
		httpClient = &cloned
	}
	httpClient.CheckRedirect = refuseRedirect
	return &Client{httpClient: httpClient, options: options}
}

// CloseIdleConnections releases pooled connections held by this Client.
func (c *Client) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func withRequestTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

func (c *Client) addCommonHeaders(req *http.Request, identity Identity) {
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")
	if value := strings.TrimSpace(c.options.AppVersion); value != "" {
		req.Header.Set("X-OpenRung-App-Version", value)
	}
	switch c.options.Platform {
	case PlatformDesktop:
		req.Header.Set("X-OpenRung-Desktop", platformHeaderValue(c.options.Platform, c.options.PlatformVersion))
	case PlatformReactNative:
		req.Header.Set("X-OpenRung-RN", platformHeaderValue(c.options.Platform, c.options.PlatformVersion))
	case PlatformAndroid:
		if value := strings.TrimSpace(c.options.PlatformVersion); value != "" {
			req.Header.Set("X-OpenRung-Android-API", value)
		}
	case PlatformIOS:
		if value := strings.TrimSpace(c.options.PlatformVersion); value != "" {
			req.Header.Set("X-OpenRung-iOS-Version", value)
		}
	}
	if strings.TrimSpace(identity.ClientID) != "" && strings.TrimSpace(identity.SessionID) != "" {
		req.Header.Set("X-OpenRung-Client-ID", identity.ClientID)
		req.Header.Set("X-OpenRung-Session-ID", identity.SessionID)
	}
}

func platformHeaderValue(platform Platform, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	switch runtime.GOOS {
	case "darwin":
		if platform == PlatformDesktop {
			return "macos"
		}
		return "ios"
	default:
		return runtime.GOOS
	}
}

// DefaultBrokerURLs returns a fresh copy of the built-in front order.
func DefaultBrokerURLs() []string {
	return []string{DefaultBrokerURL, CloudFrontBrokerURL}
}

// BrokerCandidates returns a de-duplicated discovery order. A genuine custom
// primary runs alone before the built-in fronts; merely persisting one of the
// defaults does not reorder them.
func BrokerCandidates(primary string) Candidates {
	defaults := DefaultBrokerURLs()
	ordered := make([]string, 0, len(defaults)+1)
	seen := make(map[string]struct{}, len(defaults)+1)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		ordered = append(ordered, value)
	}

	primary = strings.TrimSpace(primary)
	override := primary != ""
	for _, fallback := range defaults {
		if primary == strings.TrimSpace(fallback) {
			override = false
			break
		}
	}
	if override {
		add(primary)
	}
	for _, fallback := range defaults {
		add(fallback)
	}
	return Candidates{URLs: ordered, OverrideFirst: override}
}
