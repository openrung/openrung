// Package wsscore carries an opaque end-to-end byte stream through a
// relay-specific WebSocket front. It contains no relay routing, ticket
// issuance, origin authentication, replay storage, or platform UI.
package wsscore

import "time"

const (
	// ProtocolVersion is advertised in each signed relay front descriptor.
	ProtocolVersion = 1
	// BridgePath is the only public path used by the WSS transport.
	BridgePath = "/api/v1/wss-bridge"
	// Subprotocol prevents an unrelated WebSocket endpoint from being accepted.
	Subprotocol = "openrung-wss-bridge-v1"
	// Tickets are carried only in this header and never in a URL.
	TicketAuthorizationHeader = "Authorization"
	TicketBearerPrefix        = "Bearer "

	MaxFronts        = 4
	MaxFrontIDBytes  = 64
	MaxFrontURLBytes = 512
	// MaxTicketBytes bounds the opaque bearer value accepted by the client and
	// sidecar. Ticket creation and interpretation intentionally live elsewhere.
	MaxTicketBytes = 4096

	DefaultHandshakeTimeout = 10 * time.Second
	MaxHandshakeTimeout     = time.Minute
	DefaultPingInterval     = 30 * time.Second
	DefaultPingWriteTimeout = 10 * time.Second
	MaxPingInterval         = 10 * time.Minute
	MaxPingWriteTimeout     = time.Minute
	DefaultWebSocketReadMax = int64(1 << 20)
	MaxWebSocketReadMax     = int64(16 << 20)

	DefaultMaxConcurrentStreams = 128
	MaxConcurrentStreams        = 1024
	DefaultStreamIdleTimeout    = 5 * time.Minute
	DefaultNoStreamIdleTimeout  = 2 * time.Minute
	DefaultSessionLifetime      = 6 * time.Hour
	MaxSessionLifetime          = 24 * time.Hour
)

// Front is one signed, relay-specific CDN access path. It never names a
// destination behind the relay-local sidecar.
type Front struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocol_version"`
}

// LifecycleOptions bounds a client transport session and its local streams.
// Zero values select the safe defaults above.
type LifecycleOptions struct {
	MaxConcurrentStreams int
	StreamIdleTimeout    time.Duration
	NoStreamIdleTimeout  time.Duration
	SessionLifetime      time.Duration
}

// NormalizeLifecycleOptions applies defaults and rejects unbounded or
// nonsensical values.
func NormalizeLifecycleOptions(opts LifecycleOptions) (LifecycleOptions, error) {
	var err error
	if opts.MaxConcurrentStreams, err = boundedInt(opts.MaxConcurrentStreams, DefaultMaxConcurrentStreams, MaxConcurrentStreams, "max concurrent streams"); err != nil {
		return LifecycleOptions{}, err
	}
	if opts.StreamIdleTimeout, err = boundedDuration(opts.StreamIdleTimeout, DefaultStreamIdleTimeout, MaxSessionLifetime, "stream idle timeout"); err != nil {
		return LifecycleOptions{}, err
	}
	if opts.NoStreamIdleTimeout, err = boundedDuration(opts.NoStreamIdleTimeout, DefaultNoStreamIdleTimeout, MaxSessionLifetime, "no-stream idle timeout"); err != nil {
		return LifecycleOptions{}, err
	}
	if opts.SessionLifetime, err = boundedDuration(opts.SessionLifetime, DefaultSessionLifetime, MaxSessionLifetime, "session lifetime"); err != nil {
		return LifecycleOptions{}, err
	}
	return opts, nil
}
