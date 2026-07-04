package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"

	"openrung/internal/punch"
)

// Client is the volunteer side of the reverse tunnel. It dials the hub, performs
// the HELLO handshake, then accepts multiplexed streams from the hub and pipes
// each one to the volunteer's local (loopback) Xray listener.
type Client struct {
	// HubAddr is the hub control address (host:port).
	HubAddr string
	// TLSConfig configures the control connection to the hub. When nil the
	// client dials plaintext TCP (local development and tests only).
	TLSConfig *tls.Config
	// Hello is the handshake the volunteer announces (token + relay metadata).
	Hello HelloFrame
	// TargetHost and TargetPort point at the local Xray listener that streams
	// are piped to.
	TargetHost string
	TargetPort int
	// DialTimeout bounds the control dial. Defaults to 10s.
	DialTimeout time.Duration
	// HandshakeTimeout bounds the HELLO/HELLO_ACK exchange. Defaults to 10s.
	HandshakeTimeout time.Duration
	// ReconnectMin and ReconnectMax bound the exponential reconnect backoff.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// OnRegistered, when set, is called each time the hub accepts the tunnel,
	// with the allocated public endpoint.
	OnRegistered func(HelloAckFrame)
}

// Run maintains the tunnel until ctx is cancelled, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) error {
	logger := c.logger()
	backoff := c.reconnectMin()
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Warn("tunnel disconnected", "error", err, "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > c.reconnectMax() {
			backoff = c.reconnectMax()
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	defer conn.Close()

	hello := c.Hello
	hello.ProtocolVersion = ProtocolVersion

	_ = conn.SetDeadline(time.Now().Add(c.handshakeTimeout()))
	if err := writeFrame(conn, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	var ack HelloAckFrame
	if err := readFrame(conn, &ack); err != nil {
		return fmt.Errorf("read hello ack: %w", err)
	}
	if !ack.OK {
		msg := ack.Error
		if msg == "" {
			msg = "tunnel rejected"
		}
		return fmt.Errorf("hub rejected tunnel: %s", msg)
	}
	_ = conn.SetDeadline(time.Time{})

	if c.OnRegistered != nil {
		c.OnRegistered(ack)
	}
	c.logger().Info("tunnel established", "public_host", ack.PublicHost, "public_port", ack.PublicPort, "relay_id", ack.RelayID)

	session, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return fmt.Errorf("start yamux client: %w", err)
	}
	defer session.Close()

	go func() {
		<-ctx.Done()
		_ = session.Close()
	}()

	typed := ack.StreamTyping
	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept tunnel stream: %w", err)
		}
		go c.handleAccepted(ctx, stream, typed)
	}
}

// handleAccepted dispatches a hub-opened stream. When stream typing was
// negotiated it reads the one-byte discriminator; otherwise the stream is raw
// client traffic (legacy behaviour).
func (c *Client) handleAccepted(ctx context.Context, stream net.Conn, typed bool) {
	if !typed {
		c.handleStream(ctx, stream)
		return
	}
	var b [1]byte
	_ = stream.SetReadDeadline(time.Now().Add(punchControlTimeout))
	if _, err := io.ReadFull(stream, b[:]); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	switch b[0] {
	case StreamTypeData:
		c.handleStream(ctx, stream)
	case StreamTypeControl:
		c.handlePunchControl(ctx, stream)
	default:
		c.logger().Warn("unknown tunnel stream type", "type", b[0])
		_ = stream.Close()
	}
}

// handlePunchControl reads a punch directive, gathers the volunteer's reflexive
// candidates, replies with an ack, and lets the punch package run the direct path
// in the background (bridging to the same loopback Xray listener as tunnelled
// traffic). ctx is the long-lived tunnel context so the punched session survives
// this control stream closing but stops on volunteer shutdown.
func (c *Client) handlePunchControl(ctx context.Context, stream net.Conn) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(punchControlTimeout))
	var dir punch.PunchDirective
	if err := readFrame(stream, &dir); err != nil {
		c.logger().Warn("read punch directive failed", "error", err)
		return
	}
	_ = stream.SetReadDeadline(time.Time{})

	ack := punch.RespondToDirective(ctx, dir, c.TargetHost, c.TargetPort, c.logger())

	_ = stream.SetWriteDeadline(time.Now().Add(punchControlTimeout))
	if err := writeFrame(stream, ack); err != nil {
		c.logger().Warn("write punch ack failed", "error", err)
	}
}

func (c *Client) handleStream(ctx context.Context, stream net.Conn) {
	defer stream.Close()
	target, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.TargetHost, strconv.Itoa(c.TargetPort)))
	if err != nil {
		c.logger().Warn("dial local xray failed", "error", err)
		return
	}
	defer target.Close()
	pipe(stream, target)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, c.dialTimeout())
	defer cancel()
	if c.TLSConfig != nil {
		return (&tls.Dialer{Config: c.TLSConfig}).DialContext(dialCtx, "tcp", c.HubAddr)
	}
	return (&net.Dialer{}).DialContext(dialCtx, "tcp", c.HubAddr)
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Client) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 10 * time.Second
}

func (c *Client) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return 10 * time.Second
}

func (c *Client) reconnectMin() time.Duration {
	if c.ReconnectMin > 0 {
		return c.ReconnectMin
	}
	return time.Second
}

func (c *Client) reconnectMax() time.Duration {
	if c.ReconnectMax > 0 {
		return c.ReconnectMax
	}
	return 30 * time.Second
}
