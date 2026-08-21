package client

import (
	"context"

	"github.com/openrung/openrung/brokerapi"
)

// WSSTicketURL resolves the fixed HTTPS broker endpoint. Cleartext is allowed
// only by the shared loopback-development exception in EnforceSecureBrokerURL.
func WSSTicketURL(baseURL string) (string, error) {
	return brokerapi.WSSTicketURL(baseURL)
}

// RequestWSSSessionTicket asks a broker front for one relay/front-bound
// credential. Redirects are always returned as errors so a 307/308 cannot
// forward the POST or identity headers to a different origin (or downgrade
// HTTPS). The caller owns bounded multi-front failover and Retry-After policy.
func (c BrokerClient) RequestWSSSessionTicket(
	ctx context.Context,
	ticketRequest brokerapi.WSSTicketRequest,
	clientID string,
	sessionID string,
) (brokerapi.WSSTicketResponse, error) {
	// Inject the identity and pass the request through whole, so a field
	// added to WSSTicketRequest can never be silently zeroed on this path.
	ticketRequest.Identity = brokerapi.Identity{
		ClientID:  clientID,
		SessionID: sessionID,
	}
	return brokerapi.NewClient(c.HTTPClient, brokerapi.Options{
		AppVersion: AppVersion(),
		Platform:   c.Platform,
	}).RequestWSSTicket(ctx, c.BaseURL, ticketRequest)
}

// WSSTicketStatusError aliases brokerapi's typed ticket status error for this
// package's callers; the engine's WSS fallback ladder matches on it to decide
// front failover and Retry-After handling.
type WSSTicketStatusError = brokerapi.WSSTicketStatusError
