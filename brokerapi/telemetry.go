// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	TelemetrySchemaVersion = 1
	MaxTelemetryBodyBytes  = 512 << 10
	MaxTelemetryEvents     = 200
)

// TelemetryEvent is the broker's versioned client event wire contract.
type TelemetryEvent struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	Event         string    `json:"event"`
	OccurredAt    time.Time `json:"occurred_at"`
	ClientID      string    `json:"client_id"`
	SessionID     string    `json:"session_id"`
	RelayID       string    `json:"relay_id,omitempty"`
	Application   string    `json:"application_package,omitempty"`
	ApplicationID int       `json:"application_uid,omitempty"`
	// Destination address, port, and protocol are intentionally absent.
	// Pairing persistent client identity with browsing destinations is a
	// privacy hazard; SendTelemetryBatchJSON also scrubs those legacy fields.
	Attributes   map[string]string `json:"attributes,omitempty"`
	Measurements map[string]int64  `json:"measurements,omitempty"`
}

type telemetryBatch struct {
	Events []TelemetryEvent `json:"events"`
}

// SendTelemetry serializes and posts a batch. Empty batches are a no-op.
// Identity headers are derived from the events and are emitted only when every
// event carries the same complete client/session pair.
func (c *Client) SendTelemetry(ctx context.Context, brokerURL string, events []TelemetryEvent) error {
	if len(events) == 0 {
		return nil
	}
	if len(events) > MaxTelemetryEvents {
		return fmt.Errorf("telemetry batch has %d events, maximum is %d", len(events), MaxTelemetryEvents)
	}
	identity, err := telemetryIdentity(events)
	if err != nil {
		return err
	}
	body, err := json.Marshal(telemetryBatch{Events: events})
	if err != nil {
		return fmt.Errorf("encode telemetry batch: %w", err)
	}
	return c.sendTelemetryJSON(ctx, brokerURL, body, identity)
}

// SendTelemetryBatchJSON is a binding-friendly bridge for clients that already
// hold the standard {"events":[...]} wire JSON. It validates the same size,
// count, and identity invariants before sending.
func (c *Client) SendTelemetryBatchJSON(ctx context.Context, brokerURL string, body []byte) error {
	if len(body) > MaxTelemetryBodyBytes {
		return fmt.Errorf("telemetry body exceeds %d bytes", MaxTelemetryBodyBytes)
	}
	var batch telemetryBatch
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&batch); err != nil {
		return fmt.Errorf("decode telemetry batch: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	return c.SendTelemetry(ctx, brokerURL, batch.Events)
}

func telemetryIdentity(events []TelemetryEvent) (Identity, error) {
	first := Identity{ClientID: events[0].ClientID, SessionID: events[0].SessionID}
	for _, event := range events[1:] {
		if event.ClientID != first.ClientID || event.SessionID != first.SessionID {
			return Identity{}, errors.New("telemetry batch mixes client or session identities")
		}
	}
	return first, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("decode telemetry batch: multiple JSON values")
		}
		return fmt.Errorf("decode telemetry batch: %w", err)
	}
	return nil
}

func (c *Client) sendTelemetryJSON(ctx context.Context, brokerURL string, body []byte, identity Identity) error {
	if len(body) > MaxTelemetryBodyBytes {
		return fmt.Errorf("telemetry body exceeds %d bytes", MaxTelemetryBodyBytes)
	}
	endpoint, err := TelemetryURL(brokerURL)
	if err != nil {
		return err
	}
	ctx, cancel := withRequestTimeout(ctx, DefaultTelemetryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.addCommonHeaders(req, identity)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return statusError(resp, "telemetry", brokerURL)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
