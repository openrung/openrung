package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openrung/internal/relay"
)

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

// brokerRegistrar registers relays over the broker's HTTP API using the same
// Bearer token volunteers use, reusing the relay.RegisterRequest/Descriptor
// shapes from the existing register and heartbeat endpoints.
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
	return &brokerRegistrar{
		brokerURL: strings.TrimRight(brokerURL, "/"),
		token:     token,
		client:    client,
	}
}

func (b *brokerRegistrar) Register(ctx context.Context, req relay.RegisterRequest) (RelayRegistration, error) {
	var desc relay.Descriptor
	if err := b.postJSON(ctx, "/api/v1/volunteers/register", req, &desc); err != nil {
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
	return b.postJSON(ctx, "/api/v1/volunteers/"+relayID+"/heartbeat", map[string]bool{"ok": true}, &resp)
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
		var apiErr relay.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		return fmt.Errorf("broker %s: %s", path, apiErr.Error)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}
	return nil
}
