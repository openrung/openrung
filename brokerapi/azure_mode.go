package brokerapi

import (
	"context"
	"net/http"
)

type azureSNIContextKey struct{}

// WithAzureSNI selects normal SNI and hostname verification for Azure requests
// made with this package's HTTP clients. Other hosts retain their TLS policy.
// Keep this mode with a verified discovery winner when sending telemetry.
// Passing false explicitly restores the legacy no-SNI mode.
func WithAzureSNI(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, azureSNIContextKey{}, enabled)
}

// AzureSNIRequested reports the selected mode, including to custom transports.
func AzureSNIRequested(ctx context.Context) bool {
	value, _ := ctx.Value(azureSNIContextKey{}).(bool)
	return value
}

// Different TLS authentication modes must never share pooled connections.
type azureModeTransport struct {
	legacy *http.Transport
	sni    *http.Transport
}

func (t *azureModeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if AzureSNIRequested(r.Context()) && EndpointUnboundBrokerFront(r.URL.String()) {
		return t.sni.RoundTrip(r)
	}
	return t.legacy.RoundTrip(r)
}

func (t *azureModeTransport) CloseIdleConnections() {
	t.legacy.CloseIdleConnections()
	t.sni.CloseIdleConnections()
}
