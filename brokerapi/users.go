// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// User enrollment and authentication (spec "openrung-user-auth-v1").
//
// Every client installation holds a P-256 ECDSA keypair. The broker issues a
// stable user ID for the public key the first time it sees it and returns the
// same ID on every later authentication, so enrollment and authentication are
// one operation. Possession of the private key is proven by signing a
// short-lived, single-use broker challenge; the statement binds the challenge
// to the canonical public key, so a captured signature cannot authenticate
// another key or be replayed after the challenge is consumed.
//
// A successful authentication returns a bearer access token that later
// control-plane requests present instead of repeating the challenge dance.
// The token is opaque to clients; only the broker reads it.

const (
	// UserAuthSpecV1 is the domain-separation tag of the signed statement.
	UserAuthSpecV1 = "openrung-user-auth-v1"

	// UserStatusEnabled and UserStatusDisabled are the two user states the
	// broker reports. A disabled user still authenticates (so the client can
	// learn its state) but receives no access token.
	UserStatusEnabled  = "enabled"
	UserStatusDisabled = "disabled"

	// MaxUserChallengeBytes bounds a broker challenge. The broker issues 43
	// characters; the cap leaves room for a different encoding without
	// letting a hostile front hand a client an unbounded string to sign.
	MaxUserChallengeBytes = 128

	DefaultUserAuthTimeout = 10 * time.Second

	maxUserAuthResponseBytes = 16 << 10
	maxAccessTokenBytes      = 4096
	userSignatureBytes       = 64
	userCoordinateBytes      = 32
)

// UserChallenge is the broker's answer to a challenge request: an opaque
// string to sign and the moment it stops being accepted.
type UserChallenge struct {
	Challenge string    `json:"challenge"`
	ExpiresAt time.Time `json:"expires_at"`
}

// UserAuthRequest authenticates one installation key. PublicKey is the
// standard-base64 SubjectPublicKeyInfo (DER) of a P-256 key; Signature is the
// standard base64 of the raw 64-byte r‖s ECDSA signature (the IEEE P1363
// form WebCrypto, CryptoKit and the Go helpers here produce) over the SHA-256
// digest of UserAuthStatement.
type UserAuthRequest struct {
	PublicKey string `json:"public_key"`
	Challenge string `json:"challenge"`
	Signature string `json:"signature"`
}

// UserAuthResponse is the validated broker answer. AccessToken is empty for a
// disabled user; the broker then also answers 403, so clients normally see it
// only through UserAuthStatusError.
type UserAuthResponse struct {
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	AccessToken    string    `json:"access_token"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
}

// String keeps the bearer token out of ordinary formatted logs.
func (r UserAuthResponse) String() string {
	return fmt.Sprintf(
		"UserAuthResponse{UserID:%q, Status:%q, AccessToken:<redacted>, TokenExpiresAt:%s}",
		r.UserID, r.Status, r.TokenExpiresAt.Format(time.RFC3339),
	)
}

// GoString also redacts the bearer for %#v formatting.
func (r UserAuthResponse) GoString() string {
	return r.String()
}

// UserAuthStatusError deliberately omits the response body and any token.
// RetryAfter is set from a 429 enrollment-budget answer.
type UserAuthStatusError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *UserAuthStatusError) Error() string {
	return fmt.Sprintf("broker user authentication failed with HTTP status %d", e.StatusCode)
}

func (e *UserAuthStatusError) HTTPStatus() int { return e.StatusCode }

// UserChallengeURL resolves POST /api/v1/users/challenge.
func UserChallengeURL(baseURL string) (string, error) {
	return endpointURL(baseURL, "api/v1/users/challenge")
}

// UserAuthURL resolves POST /api/v1/users/auth.
func UserAuthURL(baseURL string) (string, error) {
	return endpointURL(baseURL, "api/v1/users/auth")
}

// EncodeUserPublicKey renders a P-256 public key as the standard-base64 SPKI
// DER the broker accepts in UserAuthRequest.PublicKey.
func EncodeUserPublicKey(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", errors.New("user public key must be a P-256 ECDSA key")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("encode user public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ParseUserPublicKey decodes UserAuthRequest.PublicKey and accepts exactly
// one key type: ECDSA on P-256.
func ParseUserPublicKey(encoded string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("user public key must be standard base64")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("user public key must be a DER SubjectPublicKeyInfo")
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("user public key must be a P-256 ECDSA key")
	}
	return pub, nil
}

// CanonicalUserPublicKey is the one spelling of a key the broker stores and
// binds into the signed statement: the standard base64 of the 65-byte
// uncompressed point. Two SPKI encodings of the same point (differing
// parameters or whitespace) therefore name one user, never two.
func CanonicalUserPublicKey(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", errors.New("user public key must be a P-256 ECDSA key")
	}
	point, err := pub.ECDH()
	if err != nil {
		return "", fmt.Errorf("canonicalize user public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(point.Bytes()), nil
}

// UserAuthStatement returns the exact bytes whose SHA-256 digest is signed:
// the spec tag, the challenge, and the canonical public key, newline-joined.
// ValidUserChallenge rejects challenges containing whitespace, so the join is
// unambiguous.
func UserAuthStatement(challenge, canonicalPublicKey string) []byte {
	return []byte(strings.Join([]string{UserAuthSpecV1, challenge, canonicalPublicKey}, "\n"))
}

// ValidUserChallenge reports whether a challenge string is one a client may
// sign and a broker may accept: non-empty, bounded, printable ASCII without
// whitespace. Both sides apply it, so neither can be made to sign or verify an
// unbounded or ambiguous statement.
func ValidUserChallenge(challenge string) bool {
	if challenge == "" || len(challenge) > MaxUserChallengeBytes {
		return false
	}
	for _, c := range []byte(challenge) {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// SignUserAuth produces the request body for a challenge with the
// installation's private key. The signature is the raw r‖s form.
func SignUserAuth(priv *ecdsa.PrivateKey, challenge string) (UserAuthRequest, error) {
	if priv == nil {
		return UserAuthRequest{}, errors.New("user private key is required")
	}
	if !ValidUserChallenge(challenge) {
		return UserAuthRequest{}, errors.New("user challenge is empty, oversized, or not printable")
	}
	encodedKey, err := EncodeUserPublicKey(&priv.PublicKey)
	if err != nil {
		return UserAuthRequest{}, err
	}
	canonical, err := CanonicalUserPublicKey(&priv.PublicKey)
	if err != nil {
		return UserAuthRequest{}, err
	}
	digest := sha256.Sum256(UserAuthStatement(challenge, canonical))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return UserAuthRequest{}, fmt.Errorf("sign user authentication: %w", err)
	}
	var raw [userSignatureBytes]byte
	r.FillBytes(raw[:userCoordinateBytes])
	s.FillBytes(raw[userCoordinateBytes:])
	return UserAuthRequest{
		PublicKey: encodedKey,
		Challenge: challenge,
		Signature: base64.StdEncoding.EncodeToString(raw[:]),
	}, nil
}

// VerifyUserAuth checks a raw r‖s signature over the statement for challenge
// and pub. It is the broker's check; clients use it in tests.
func VerifyUserAuth(pub *ecdsa.PublicKey, challenge, signature string) bool {
	if !ValidUserChallenge(challenge) {
		return false
	}
	canonical, err := CanonicalUserPublicKey(pub)
	if err != nil {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(raw) != userSignatureBytes {
		return false
	}
	r := new(big.Int).SetBytes(raw[:userCoordinateBytes])
	s := new(big.Int).SetBytes(raw[userCoordinateBytes:])
	digest := sha256.Sum256(UserAuthStatement(challenge, canonical))
	return ecdsa.Verify(pub, digest[:], r, s)
}

// RequestUserChallenge asks one endpoint-authenticated broker front for a
// challenge. The exchange is refused on an endpoint-unbound front: the
// following authentication returns a bearer token, which a relay-list
// signature cannot keep confidential from an impersonating front. The native
// Azure front counts as endpoint-bound only when the caller selected its
// hostname-verified SNI mode (WithAzureSNI); its no-SNI fallback mode, which
// authenticates a shared edge rather than the exact endpoint, is refused.
// The telemetry client identity is deliberately never attached: the user ID
// stays unlinked from the installation's telemetry ID on the wire.
func (c *Client) RequestUserChallenge(ctx context.Context, brokerURL string) (UserChallenge, error) {
	endpoint, err := UserChallengeURL(brokerURL)
	if err != nil {
		return UserChallenge{}, err
	}
	if EndpointUnboundBrokerFront(brokerURL) && !AzureSNIRequested(ctx) {
		return UserChallenge{}, errors.New("user authentication requires TLS authentication of the exact broker endpoint")
	}
	body, err := c.postUserAuth(ctx, endpoint, nil)
	if err != nil {
		return UserChallenge{}, err
	}
	var challenge UserChallenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		return UserChallenge{}, errors.New("decode user challenge response")
	}
	if !ValidUserChallenge(challenge.Challenge) {
		return UserChallenge{}, errors.New("user challenge response has a missing, oversized, or invalid challenge")
	}
	if challenge.ExpiresAt.IsZero() || !challenge.ExpiresAt.After(time.Now()) {
		return UserChallenge{}, errors.New("user challenge response is missing expires_at or already expired")
	}
	return challenge, nil
}

// AuthenticateUser presents a signed challenge and returns the user's ID,
// status, and access token. Non-2xx answers surface as *UserAuthStatusError
// with the broker's Retry-After when it sent one.
func (c *Client) AuthenticateUser(ctx context.Context, brokerURL string, request UserAuthRequest) (UserAuthResponse, error) {
	if strings.TrimSpace(request.PublicKey) == "" || strings.TrimSpace(request.Signature) == "" {
		return UserAuthResponse{}, errors.New("user authentication requires public_key and signature")
	}
	if !ValidUserChallenge(request.Challenge) {
		return UserAuthResponse{}, errors.New("user challenge is empty, oversized, or not printable")
	}
	endpoint, err := UserAuthURL(brokerURL)
	if err != nil {
		return UserAuthResponse{}, err
	}
	if EndpointUnboundBrokerFront(brokerURL) && !AzureSNIRequested(ctx) {
		return UserAuthResponse{}, errors.New("user authentication requires TLS authentication of the exact broker endpoint")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return UserAuthResponse{}, fmt.Errorf("encode user authentication request: %w", err)
	}
	body, err := c.postUserAuth(ctx, endpoint, payload)
	if err != nil {
		return UserAuthResponse{}, err
	}
	var response UserAuthResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return UserAuthResponse{}, errors.New("decode user authentication response")
	}
	if strings.TrimSpace(response.UserID) == "" {
		return UserAuthResponse{}, errors.New("user authentication response has no user_id")
	}
	if response.Status != UserStatusEnabled && response.Status != UserStatusDisabled {
		return UserAuthResponse{}, errors.New("user authentication response has an unknown status")
	}
	if response.Status == UserStatusEnabled {
		if size := len(response.AccessToken); size < 1 || size > maxAccessTokenBytes || strings.ContainsAny(response.AccessToken, " \r\n") {
			return UserAuthResponse{}, errors.New("user authentication response has a missing, oversized, or invalid access token")
		}
		if response.TokenExpiresAt.IsZero() || !response.TokenExpiresAt.After(time.Now()) {
			return UserAuthResponse{}, errors.New("user authentication response token is missing expires_at or already expired")
		}
	}
	return response, nil
}

// postUserAuth is the one request path for both user endpoints: bounded
// timeout, no redirects (refuseRedirect on the client), no identity headers,
// a status error without body on non-2xx, and a size-capped body otherwise.
func (c *Client) postUserAuth(ctx context.Context, endpoint string, payload []byte) ([]byte, error) {
	ctx, cancel := withRequestTimeout(ctx, DefaultUserAuthTimeout)
	defer cancel()
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("create user authentication request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.addCommonHeaders(req, Identity{})

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request user authentication: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &UserAuthStatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserAuthResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read user authentication response: %w", err)
	}
	if len(body) > maxUserAuthResponseBytes {
		return nil, fmt.Errorf("user authentication response exceeds %d bytes", maxUserAuthResponseBytes)
	}
	return body, nil
}
