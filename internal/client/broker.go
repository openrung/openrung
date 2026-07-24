package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

type BrokerClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Platform   brokerapi.Platform
}

// ListRelays fetches relay candidates from the broker. When clientID and
// sessionID are non-empty they are sent as identity headers so the broker can
// auto-record a client_seen telemetry event for the request. Successful
// responses are signature-verified by brokerapi before this compatibility
// adapter decodes them into the application's relay model.
func (c BrokerClient) ListRelays(ctx context.Context, limit int, clientID, sessionID string) (relay.ListResponse, error) {
	list, err := brokerapi.NewClient(c.HTTPClient, brokerapi.Options{
		AppVersion: AppVersion(),
		Platform:   c.Platform,
	}).ListRelays(ctx, c.BaseURL, brokerapi.ListOptions{
		Limit: limit,
		Identity: brokerapi.Identity{
			ClientID:  clientID,
			SessionID: sessionID,
		},
	})
	if err != nil {
		return relay.ListResponse{}, err
	}

	var response relay.ListResponse
	if err := json.Unmarshal(list.JSON(), &response); err != nil {
		return relay.ListResponse{}, fmt.Errorf("decode verified relay list: %w", err)
	}
	return response, nil
}

func RelayListURL(baseURL string, limit int) (string, error) {
	return brokerapi.RelayListURL(baseURL, limit)
}

// EnforceSecureBrokerURL parses baseURL and rejects cleartext broker endpoints.
// https is allowed to any host; plain http is allowed ONLY to a loopback host
// (local development). Plaintext http to any other host is refused so an on-path
// network observer cannot read or tamper with the pre-tunnel broker traffic —
// the relay directory seeds the entire VPN config and both the directory and
// telemetry requests carry the persistent client identity headers. It returns
// the parsed URL so callers can build endpoint paths on it.
//
// (A pinned bare-IP HTTPS fallback for a blocked edge can be layered on later;
// until then there is no cleartext path off the device.)
func EnforceSecureBrokerURL(baseURL string) (*url.URL, error) {
	return brokerapi.EnforceSecureBrokerURL(baseURL)
}

// BrokerStatusError remains as a compatibility alias for callers that classify
// broker errors by HTTP status.
type BrokerStatusError = brokerapi.BrokerStatusError
