// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client access (ADR-006 "Client enrollment and relay authorization").
//
// An authenticated user asks the broker for relays to connect to. The broker
// issues (or extends) one short-lived VLESS credential per returned relay,
// waits until each relay has installed it, and answers with a signed body in
// the relay-list shape whose client_id fields are those per-user credentials.
// A renewal is the same request naming the connected relay: the broker keeps
// the credential and moves lease_expires_at forward.

const (
	// MaxAccessRelays bounds the relays one access request may ask for.
	MaxAccessRelays = 5

	// DefaultAccessTimeout covers the broker's wait for relays to install the
	// issued credentials (up to 5 s) plus a round trip through a CDN front.
	DefaultAccessTimeout = 20 * time.Second

	maxAccessResponseBytes = 1 << 20
)

// AccessRequest asks for relays (and per-user credentials on them). RelayID
// pins one relay, Country narrows to an ISO country code; at most one is set,
// and neither asks for the best-ranked relays. Limit 0 means MaxAccessRelays.
// AccessToken is the bearer from AuthenticateUser and never part of the body.
type AccessRequest struct {
	Limit       int    `json:"limit,omitempty"`
	RelayID     string `json:"relay_id,omitempty"`
	Country     string `json:"country,omitempty"`
	AccessToken string `json:"-"`
}

// String keeps the bearer token out of ordinary formatted logs.
func (r AccessRequest) String() string {
	return fmt.Sprintf("AccessRequest{Limit:%d, RelayID:%q, Country:%q, AccessToken:<redacted>}", r.Limit, r.RelayID, r.Country)
}

// GoString also redacts the bearer for %#v formatting.
func (r AccessRequest) GoString() string { return r.String() }

// AccessResponse is the verified access answer: a relay list whose client_id
// values are the caller's lease credentials, valid until LeaseExpiresAt (the
// broker's clock; compare against ServerTime, not the local clock).
type AccessResponse struct {
	RelayListResponse
	// LeaseExpiresAt is zero only when Relays is empty.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// AccessStatusError reports a non-2xx access answer without its body. 401
// means the access token is no longer accepted (authenticate again), 403 that
// the user is disabled, 404 that the broker has no access endpoint, and 503
// with RetryAfter that the credentials were issued but not yet installed.
type AccessStatusError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *AccessStatusError) Error() string {
	return fmt.Sprintf("broker access request failed with HTTP status %d", e.StatusCode)
}

func (e *AccessStatusError) HTTPStatus() int { return e.StatusCode }

// RequestAccess asks one endpoint-authenticated broker front for relays and
// per-user credentials. Like the user endpoints it is refused on an
// endpoint-unbound front unless the caller selected Azure's hostname-verified
// SNI mode (WithAzureSNI): the response carries credentials a relay-list
// signature cannot keep confidential from an impersonating front. Redirects
// are never followed and no telemetry identity is attached, so the user stays
// unlinked from the installation's telemetry ID on the wire.
func (c *Client) RequestAccess(ctx context.Context, brokerURL string, request AccessRequest) (AccessResponse, error) {
	token := strings.TrimSpace(request.AccessToken)
	if token == "" || len(token) > maxAccessTokenBytes || strings.ContainsAny(token, " \r\n") {
		return AccessResponse{}, errors.New("access request requires a valid access token")
	}
	if request.Limit < 0 || request.Limit > MaxAccessRelays {
		return AccessResponse{}, fmt.Errorf("access limit must be between 1 and %d", MaxAccessRelays)
	}
	if strings.TrimSpace(request.RelayID) != "" && strings.TrimSpace(request.Country) != "" {
		return AccessResponse{}, errors.New("access request targets either a relay or a country, not both")
	}
	endpoint, err := AccessURL(brokerURL)
	if err != nil {
		return AccessResponse{}, err
	}
	if EndpointUnboundBrokerFront(brokerURL) && !AzureSNIRequested(ctx) {
		return AccessResponse{}, errors.New("access requests require TLS authentication of the exact broker endpoint")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return AccessResponse{}, fmt.Errorf("encode access request: %w", err)
	}

	ctx, cancel := withRequestTimeout(ctx, DefaultAccessTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return AccessResponse{}, fmt.Errorf("create access request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	c.addCommonHeaders(req, Identity{})

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AccessResponse{}, fmt.Errorf("request access: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return AccessResponse{}, &AccessStatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAccessResponseBytes+1))
	if err != nil {
		return AccessResponse{}, fmt.Errorf("read access response: %w", err)
	}
	if len(body) > maxAccessResponseBytes {
		return AccessResponse{}, fmt.Errorf("access response exceeds %d bytes", maxAccessResponseBytes)
	}
	return verifyAccessResponse(productionRelayKeys, endpoint, relaySignatureHeaderValue(resp.Header), body, time.Now())
}

type accessEnvelope struct {
	Channel        string    `json:"channel"`
	NotAfter       time.Time `json:"not_after"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// verifyAccessResponse is verifyRelayList for the access channel: the same
// pinned keys, detached signature, freshness bound and wire schema, but the
// channel must be ChannelAccess and lease_expires_at takes the place of the
// echoed limit. Loopback endpoints keep the development exception (schema
// only, no signature), exactly like the relay list.
func verifyAccessResponse(keys []pinnedKey, endpoint, signatureHeader string, body []byte, now time.Time) (AccessResponse, error) {
	if !endpointIsLoopback(endpoint) {
		headerKeyID, signature, err := parseRelaySignatureHeader(signatureHeader)
		if err != nil {
			return AccessResponse{}, err
		}
		verified := false
		for _, candidate := range orderKeysByAdvisoryID(keys, headerKeyID) {
			if ed25519.Verify(candidate.key, body, signature) {
				verified = true
				break
			}
		}
		if !verified {
			return AccessResponse{}, verificationFailure("signature does not verify under any pinned key (header key_id %q)", headerKeyID)
		}
		var envelope accessEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			return AccessResponse{}, verificationFailure("signed body is not valid JSON: %v", err)
		}
		if envelope.Channel != ChannelAccess {
			return AccessResponse{}, verificationFailure("channel %q does not match the %q channel this response was requested from", envelope.Channel, ChannelAccess)
		}
		if envelope.NotAfter.IsZero() {
			return AccessResponse{}, verificationFailure("signed body carries no not_after freshness bound")
		}
		if envelope.NotAfter.Before(now.Add(-notAfterSkewAllowance)) {
			return AccessResponse{}, verificationFailure(
				"access response expired: not_after %s is past even with the %s clock-skew allowance (local time %s)",
				envelope.NotAfter.Format(time.RFC3339), notAfterSkewAllowance, now.Format(time.RFC3339),
			)
		}
	}
	if err := validateRelayListWire(body); err != nil {
		return AccessResponse{}, verificationFailure("access body does not match the relay-list wire schema: %v", err)
	}
	var response AccessResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return AccessResponse{}, fmt.Errorf("decode access response: %w", err)
	}
	if len(response.Relays) > MaxAccessRelays {
		return AccessResponse{}, verificationFailure("access response lists %d relays, more than the %d a request may ask for", len(response.Relays), MaxAccessRelays)
	}
	if len(response.Relays) > 0 && !response.LeaseExpiresAt.After(response.ServerTime) {
		return AccessResponse{}, verificationFailure("access response has relays but no lease_expires_at after server_time")
	}
	return response, nil
}
