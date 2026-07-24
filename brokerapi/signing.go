// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// RelaySignatureHeader carries
	// "ed25519;<key_id>;<standard-base64 signature>" over exact body bytes.
	RelaySignatureHeader = "X-OpenRung-Relays-Signature"

	relaySigningKeyActiveHex  = "176c03cbc70833285abcea75f2a0e137bd687629142408c22806a86308bd4974"
	relaySigningKeyStandbyHex = "5b2698cfa7a796c671a30aabd5475d55095b91464221f051837eb8fe01f36ea2"

	relayChannelAPI       = "api"
	notAfterSkewAllowance = 5 * time.Minute
)

type pinnedKey struct {
	id  string
	key ed25519.PublicKey
}

var productionRelayKeys = mustPinnedKeys(relaySigningKeyActiveHex, relaySigningKeyStandbyHex)

func mustPinnedKeys(keysHex ...string) []pinnedKey {
	keys := make([]pinnedKey, 0, len(keysHex))
	for _, keyHex := range keysHex {
		raw, err := hex.DecodeString(keyHex)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("pinned relay signing key %q is not 32 hex-encoded bytes", keyHex))
		}
		key := ed25519.PublicKey(bytes.Clone(raw))
		keys = append(keys, pinnedKey{id: signingKeyID(key), key: key})
	}
	return keys
}

func signingKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

type relayListEnvelope struct {
	Channel  string    `json:"channel"`
	Limit    int       `json:"limit"`
	NotAfter time.Time `json:"not_after"`
}

func verifyRelayList(
	keys []pinnedKey,
	endpoint string,
	requestedLimit int,
	signatureHeader string,
	body []byte,
	now time.Time,
) (RelayList, error) {
	if endpointIsLoopback(endpoint) {
		if err := validateRelayListWire(body); err != nil {
			return RelayList{}, err
		}
		return RelayList{rawJSON: bytes.Clone(body)}, nil
	}

	headerKeyID, signature, err := parseRelaySignatureHeader(signatureHeader)
	if err != nil {
		return RelayList{}, err
	}
	verifiedKeyID := ""
	for _, candidate := range orderKeysByAdvisoryID(keys, headerKeyID) {
		if ed25519.Verify(candidate.key, body, signature) {
			verifiedKeyID = candidate.id
			break
		}
	}
	if verifiedKeyID == "" {
		return RelayList{}, verificationFailure("signature does not verify under any pinned key (header key_id %q)", headerKeyID)
	}

	var envelope relayListEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return RelayList{}, verificationFailure("signed body is not valid JSON: %v", err)
	}
	if envelope.Channel != relayChannelAPI {
		return RelayList{}, verificationFailure("channel %q does not match the %q channel this candidate was fetched from", envelope.Channel, relayChannelAPI)
	}
	if envelope.Limit != effectiveRelayLimit(requestedLimit) {
		return RelayList{}, verificationFailure("echoed limit %d does not match requested limit %d", envelope.Limit, effectiveRelayLimit(requestedLimit))
	}
	if envelope.NotAfter.IsZero() {
		return RelayList{}, verificationFailure("signed body carries no not_after freshness bound")
	}
	if envelope.NotAfter.Before(now.Add(-notAfterSkewAllowance)) {
		return RelayList{}, verificationFailure(
			"list expired: not_after %s is past even with the %s clock-skew allowance (local time %s)",
			envelope.NotAfter.Format(time.RFC3339),
			notAfterSkewAllowance,
			now.Format(time.RFC3339),
		)
	}
	if err := validateRelayListWire(body); err != nil {
		return RelayList{}, verificationFailure("signed body does not match the relay-list wire schema: %v", err)
	}
	return RelayList{
		rawJSON:           bytes.Clone(body),
		KeyID:             verifiedKeyID,
		SignatureVerified: true,
	}, nil
}

func parseRelaySignatureHeader(header string) (keyID string, signature []byte, err error) {
	if header == "" {
		return "", nil, verificationFailure(
			"response carries no %s header — this broker has not enabled relay-list signing (this build requires a signing broker; plain-JSON responses are accepted only from loopback broker URLs for development)",
			RelaySignatureHeader,
		)
	}
	fields := strings.Split(header, ";")
	if len(fields) != 3 {
		return "", nil, verificationFailure("malformed %s header: want 3 ';'-separated fields, got %d", RelaySignatureHeader, len(fields))
	}
	if fields[0] != "ed25519" {
		return "", nil, verificationFailure("unsupported signature algorithm %q (want ed25519)", fields[0])
	}
	signature, decodeErr := base64.StdEncoding.DecodeString(fields[2])
	if decodeErr != nil {
		return "", nil, verificationFailure("signature is not valid standard base64: %v", decodeErr)
	}
	if len(signature) != ed25519.SignatureSize {
		return "", nil, verificationFailure("signature is %d bytes, want %d", len(signature), ed25519.SignatureSize)
	}
	return fields[1], signature, nil
}

func orderKeysByAdvisoryID(keys []pinnedKey, headerKeyID string) []pinnedKey {
	ordered := make([]pinnedKey, 0, len(keys))
	for _, key := range keys {
		if key.id == headerKeyID {
			ordered = append(ordered, key)
		}
	}
	for _, key := range keys {
		if key.id != headerKeyID {
			ordered = append(ordered, key)
		}
	}
	return ordered
}

func relaySignatureHeaderValue(header http.Header) string {
	if value := header.Get(RelaySignatureHeader); value != "" {
		return value
	}
	for name, values := range header {
		if strings.EqualFold(name, RelaySignatureHeader) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
