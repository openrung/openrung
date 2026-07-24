package clienttelemetry

import (
	"context"
	"net/http"

	"github.com/openrung/openrung/brokerapi"
)

// HTTPClient posts telemetry batches to the broker. It is the CLI analog of the
// Android TelemetryClient.
type HTTPClient struct {
	BaseURL    string
	HTTP       *http.Client
	AppVersion string
	Platform   brokerapi.Platform
}

// Send posts the given events to POST /api/v1/telemetry/events. It is a no-op for
// an empty slice. brokerapi derives identity headers from a complete,
// single-identity batch and rejects mixed client/session identities.
func (c HTTPClient) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	api := brokerapi.NewClient(c.HTTP, brokerapi.Options{
		AppVersion: c.AppVersion,
		Platform:   c.Platform,
	})
	return api.SendTelemetry(ctx, c.BaseURL, events)
}

// TelemetryURL builds the telemetry endpoint URL from a broker base URL,
// mirroring RelayListURL in internal/client/broker.go. Like the relay-list path
// it refuses cleartext endpoints so pre-tunnel telemetry (which carries the
// persistent client identity) never leaves the device in the clear.
func TelemetryURL(baseURL string) (string, error) {
	return brokerapi.TelemetryURL(baseURL)
}
