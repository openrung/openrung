package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrung/internal/relay"
)

const (
	maxWSSTicketResponseBytes = 64 << 10
)

// WSSTicketURL resolves the fixed HTTPS broker endpoint. Cleartext is allowed
// only by the shared loopback-development exception in EnforceSecureBrokerURL.
func WSSTicketURL(baseURL string) (string, error) {
	parsed, err := EnforceSecureBrokerURL(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.Trim(parsed.Path, "/")
	parts := []string{"api/v1/wss/tickets"}
	if basePath != "" {
		parts = append([]string{basePath}, parts...)
	}
	parsed.Path = "/" + strings.Join(parts, "/")
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

// RequestWSSSessionTicket asks a broker front for one relay/front-bound
// credential. Redirects are always returned as errors so a 307/308 cannot
// forward the POST or identity headers to a different origin (or downgrade
// HTTPS). The caller owns bounded multi-front failover and Retry-After policy.
func (c BrokerClient) RequestWSSSessionTicket(
	ctx context.Context,
	ticketRequest relay.WSSSessionTicketRequest,
	clientID string,
	sessionID string,
) (relay.WSSSessionTicketResponse, error) {
	if strings.TrimSpace(ticketRequest.RelayID) == "" {
		return relay.WSSSessionTicketResponse{}, errors.New("WSS ticket relay_id is required")
	}
	if strings.TrimSpace(ticketRequest.FrontID) == "" {
		return relay.WSSSessionTicketResponse{}, errors.New("WSS ticket front_id is required")
	}
	endpoint, err := WSSTicketURL(c.BaseURL)
	if err != nil {
		return relay.WSSSessionTicketResponse{}, err
	}
	payload, err := json.Marshal(ticketRequest)
	if err != nil {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("encode WSS ticket request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("create WSS ticket request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("X-OpenRung-App-Version", AppVersion())
	if clientID != "" && sessionID != "" {
		req.Header.Set("X-OpenRung-Client-ID", clientID)
		req.Header.Set("X-OpenRung-Session-ID", sessionID)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	noRedirectClient := *httpClient
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("request WSS ticket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return relay.WSSSessionTicketResponse{}, wssTicketStatusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWSSTicketResponseBytes+1))
	if err != nil {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("read WSS ticket response: %w", err)
	}
	if len(body) > maxWSSTicketResponseBytes {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("WSS ticket response exceeds %d bytes", maxWSSTicketResponseBytes)
	}
	var ticket relay.WSSSessionTicketResponse
	if err := json.Unmarshal(body, &ticket); err != nil {
		return relay.WSSSessionTicketResponse{}, fmt.Errorf("decode WSS ticket response: %w", err)
	}
	if strings.TrimSpace(ticket.Ticket) == "" {
		return relay.WSSSessionTicketResponse{}, errors.New("WSS ticket response has no ticket")
	}
	if ticket.ExpiresAt.IsZero() {
		return relay.WSSSessionTicketResponse{}, errors.New("WSS ticket response has no expires_at")
	}
	if strings.TrimSpace(ticket.URL) == "" {
		return relay.WSSSessionTicketResponse{}, errors.New("WSS ticket response has no url")
	}
	return ticket, nil
}

// WSSTicketStatusError retains the status and bounded Retry-After hint needed
// for front failover without including request bodies, tickets, or URLs.
type WSSTicketStatusError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *WSSTicketStatusError) Error() string {
	return fmt.Sprintf("broker WSS ticket request failed with HTTP status %d", e.StatusCode)
}
func (e *WSSTicketStatusError) HTTPStatus() int { return e.StatusCode }

func wssTicketStatusError(resp *http.Response) error {
	return &WSSTicketStatusError{
		StatusCode: resp.StatusCode,
		RetryAfter: parseWSSTicketRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

func parseWSSTicketRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 || seconds > math.MaxInt64/int64(time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
