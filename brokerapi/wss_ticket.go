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
	"strings"
	"time"
)

const (
	maxWSSTicketResponseBytes = 64 << 10
	maxWSSTicketBytes         = 4096
)

// WSSTicketRequest selects one relay/front-bound credential. Identity is used
// for request headers and is excluded from the JSON body.
type WSSTicketRequest struct {
	RelayID  string   `json:"relay_id"`
	FrontID  string   `json:"front_id"`
	Identity Identity `json:"-"`
}

// WSSTicketResponse is the validated broker ticket response.
type WSSTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
}

// String keeps the bearer credential out of ordinary formatted logs.
func (r WSSTicketResponse) String() string {
	return fmt.Sprintf(
		"WSSTicketResponse{Ticket:<redacted>, ExpiresAt:%s, URL:%q}",
		r.ExpiresAt.Format(time.RFC3339),
		r.URL,
	)
}

// GoString also redacts the bearer for %#v formatting.
func (r WSSTicketResponse) GoString() string {
	return r.String()
}

// RequestWSSTicket asks one broker front for a credential. Redirects are never
// followed, preventing POST or identity replay to another origin.
func (c *Client) RequestWSSTicket(
	ctx context.Context,
	brokerURL string,
	ticketRequest WSSTicketRequest,
) (WSSTicketResponse, error) {
	if strings.TrimSpace(ticketRequest.RelayID) == "" {
		return WSSTicketResponse{}, errors.New("WSS ticket relay_id is required")
	}
	if strings.TrimSpace(ticketRequest.FrontID) == "" {
		return WSSTicketResponse{}, errors.New("WSS ticket front_id is required")
	}
	endpoint, err := WSSTicketURL(brokerURL)
	if err != nil {
		return WSSTicketResponse{}, err
	}
	ctx, cancel := withRequestTimeout(ctx, DefaultWSSTicketTimeout)
	defer cancel()
	payload, err := json.Marshal(struct {
		RelayID string `json:"relay_id"`
		FrontID string `json:"front_id"`
	}{
		RelayID: ticketRequest.RelayID,
		FrontID: ticketRequest.FrontID,
	})
	if err != nil {
		return WSSTicketResponse{}, fmt.Errorf("encode WSS ticket request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return WSSTicketResponse{}, fmt.Errorf("create WSS ticket request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.addCommonHeaders(req, ticketRequest.Identity)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return WSSTicketResponse{}, fmt.Errorf("request WSS ticket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return WSSTicketResponse{}, &WSSTicketStatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWSSTicketResponseBytes+1))
	if err != nil {
		return WSSTicketResponse{}, fmt.Errorf("read WSS ticket response: %w", err)
	}
	if len(body) > maxWSSTicketResponseBytes {
		return WSSTicketResponse{}, fmt.Errorf("WSS ticket response exceeds %d bytes", maxWSSTicketResponseBytes)
	}
	var ticket WSSTicketResponse
	if err := json.Unmarshal(body, &ticket); err != nil {
		return WSSTicketResponse{}, errors.New("decode WSS ticket response")
	}
	if ticketSize := len(ticket.Ticket); ticketSize < 1 || ticketSize > maxWSSTicketBytes ||
		strings.ContainsAny(ticket.Ticket, "\r\n") {
		return WSSTicketResponse{}, errors.New("WSS ticket response has a missing, oversized, or invalid ticket")
	}
	if ticket.ExpiresAt.IsZero() {
		return WSSTicketResponse{}, errors.New("WSS ticket response has no expires_at")
	}
	if !ticket.ExpiresAt.After(time.Now()) {
		return WSSTicketResponse{}, errors.New("WSS ticket response is already expired")
	}
	if strings.TrimSpace(ticket.URL) == "" {
		return WSSTicketResponse{}, errors.New("WSS ticket response has no url")
	}
	return ticket, nil
}
