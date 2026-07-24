package client

import (
	"context"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
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
	ticketRequest relay.WSSSessionTicketRequest,
	clientID string,
	sessionID string,
) (relay.WSSSessionTicketResponse, error) {
	ticket, err := brokerapi.NewClient(c.HTTPClient, brokerapi.Options{
		AppVersion: AppVersion(),
		Platform:   c.Platform,
	}).RequestWSSTicket(ctx, c.BaseURL, brokerapi.WSSTicketRequest{
		RelayID: ticketRequest.RelayID,
		FrontID: ticketRequest.FrontID,
		Identity: brokerapi.Identity{
			ClientID:  clientID,
			SessionID: sessionID,
		},
	})
	if err != nil {
		return relay.WSSSessionTicketResponse{}, err
	}
	return relay.WSSSessionTicketResponse{
		Ticket:    ticket.Ticket,
		ExpiresAt: ticket.ExpiresAt,
		URL:       ticket.URL,
	}, nil
}

// WSSTicketStatusError remains as a compatibility alias for desktop failover.
type WSSTicketStatusError = brokerapi.WSSTicketStatusError
