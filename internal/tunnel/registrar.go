package tunnel

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

	"openrung/internal/relay"
)

// ErrRelayNotFound is returned (wrapped) by Heartbeat when the broker no
// longer knows the relay — e.g. an in-memory store lost it across a broker
// restart. The caller should re-register rather than keep heartbeating.
var ErrRelayNotFound = errors.New("relay not found")

const maxBrokerErrorBodyBytes = 64 << 10

// RelayRegistration is the result of registering a tunneled relay with the broker.
type RelayRegistration struct {
	RelayID    string
	PublicHost string
	PublicPort int
	ExpiresAt  time.Time
}

// Registrar registers tunneled relays with the broker and keeps them alive. It
// is an interface so the hub can be tested without a live broker.
type Registrar interface {
	Register(ctx context.Context, req relay.RegisterRequest) (RelayRegistration, error)
	Heartbeat(ctx context.Context, relayID string) error
}

// brokerRegistrar registers relays over the broker's canonical HTTP API using
// the same bearer token volunteer-class relays use.
type brokerRegistrar struct {
	brokerURL string
	token     string
	client    *http.Client
}

// NewBrokerRegistrar builds a Registrar backed by the broker HTTP API.
func NewBrokerRegistrar(brokerURL, token string, client *http.Client) Registrar {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Broker write endpoints are canonical and must never redirect. Refusing
	// redirects prevents forwarding the bearer token to another URL.
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &brokerRegistrar{
		brokerURL: strings.TrimRight(brokerURL, "/"),
		token:     token,
		client:    &noRedirectClient,
	}
}

func (b *brokerRegistrar) Register(ctx context.Context, req relay.RegisterRequest) (RelayRegistration, error) {
	var desc relay.Descriptor
	if err := b.postJSON(ctx, "/api/v1/relays/register", req, &desc); err != nil {
		return RelayRegistration{}, err
	}
	return RelayRegistration{
		RelayID:    desc.ID,
		PublicHost: desc.PublicHost,
		PublicPort: desc.PublicPort,
		ExpiresAt:  desc.ExpiresAt,
	}, nil
}

func (b *brokerRegistrar) Heartbeat(ctx context.Context, relayID string) error {
	var resp relay.HeartbeatResponse
	return b.postJSON(ctx, "/api/v1/relays/"+relayID+"/heartbeat", map[string]bool{"ok": true}, &resp)
}

// brokerHTTPError is a non-2xx broker response.
type brokerHTTPError struct {
	path       string
	statusCode int
	message    string
}

func (e *brokerHTTPError) Error() string {
	return fmt.Sprintf("broker %s: %s", e.path, e.message)
}

func (b *brokerRegistrar) postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.brokerURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBrokerErrorBodyBytes+1))
		bodyWithinLimit := len(errorBody) <= maxBrokerErrorBodyBytes
		var apiErr relay.ErrorResponse
		if readErr == nil && bodyWithinLimit {
			_ = json.Unmarshal(errorBody, &apiErr)
		}
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		if resp.StatusCode == http.StatusNotFound && apiErr.Error == "relay not found" {
			return fmt.Errorf("broker %s: %w", path, ErrRelayNotFound)
		}
		return &brokerHTTPError{
			path:       path,
			statusCode: resp.StatusCode,
			message:    apiErr.Error,
		}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}
	return nil
}
