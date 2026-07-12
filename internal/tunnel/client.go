package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/openrung/openrung/punchcore"

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
	// OnDisconnected, when set, is called each time a tunnel attempt ends (drop
	// or failed dial/handshake) with the error and the backoff before the next
	// attempt. Not called on context cancellation.
	OnDisconnected func(err error, retryIn time.Duration)
	// Stats, when set, accumulates live data-stream counters. Punched
	// direct-path traffic bypasses the tunnel and is not counted here.
	Stats *TrafficStats
}

// TrafficStats holds live tunnel data-plane counters, safe for concurrent use.
type TrafficStats struct {
	active       atomic.Int64
	totalStreams atomic.Uint64
	fromClients  atomic.Uint64 // bytes client → xray
	toClients    atomic.Uint64 // bytes xray → client
}

// TrafficSnapshot is a point-in-time copy of TrafficStats.
type TrafficSnapshot struct {
	ActiveStreams    int64
	TotalStreams     uint64
	BytesFromClients uint64
	BytesToClients   uint64
}

func (t *TrafficStats) Snapshot() TrafficSnapshot {
	return TrafficSnapshot{
		ActiveStreams:    t.active.Load(),
		TotalStreams:     t.totalStreams.Load(),
		BytesFromClients: t.fromClients.Load(),
		BytesToClients:   t.toClients.Load(),
	}
}

// tunnelStableThreshold is how long a tunnel must stay established before Run
// treats a later drop as a fresh outage worth an immediate retry. A session
// that flaps sooner (a hub that accepts HELLO_ACK then drops the yamux session)
// keeps the growing backoff, so a flapping hub is not hammered once per second.
const tunnelStableThreshold = time.Minute

// Run maintains the tunnel until ctx is cancelled, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) error {
	logger := c.logger()
	backoff := c.reconnectMin()
	for {
		if ctx.Err() != nil {
			return nil
		}
		uptime, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if uptime >= tunnelStableThreshold {
			// The tunnel stayed up a while: this drop is a fresh outage, so
			// retry promptly instead of inheriting stale accumulated backoff.
			backoff = c.reconnectMin()
		}
		if err != nil {
			logger.Warn("tunnel disconnected", "error", err, "retry_in", backoff.String())
		}
		if c.OnDisconnected != nil {
			c.OnDisconnected(err, backoff)
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

// runOnce returns how long the tunnel stayed established (0 if the handshake
// never completed) so Run can decide whether to reset its backoff, alongside
// the terminating error.
func (c *Client) runOnce(ctx context.Context) (time.Duration, error) {
	// A per-attempt context bounds the session-close watcher goroutine below to
	// this call: without it the watcher blocks on the long-lived tunnel ctx and
	// leaks one goroutine per reconnect until the whole tunnel is torn down.
	onceCtx, cancelOnce := context.WithCancel(ctx)
	defer cancelOnce()

	conn, err := c.dial(onceCtx)
	if err != nil {
		return 0, fmt.Errorf("dial hub: %w", err)
	}
	defer conn.Close()

	hello := c.Hello
	hello.ProtocolVersion = ProtocolVersion

	_ = conn.SetDeadline(time.Now().Add(c.handshakeTimeout()))
	if err := writeFrame(conn, hello); err != nil {
		return 0, fmt.Errorf("send hello: %w", err)
	}
	var ack HelloAckFrame
	if err := readFrame(conn, &ack); err != nil {
		return 0, fmt.Errorf("read hello ack: %w", err)
	}
	if !ack.OK {
		msg := ack.Error
		if msg == "" {
			msg = "tunnel rejected"
		}
		return 0, fmt.Errorf("hub rejected tunnel: %s", msg)
	}
	_ = conn.SetDeadline(time.Time{})
	establishedAt := time.Now()

	if c.OnRegistered != nil {
		c.OnRegistered(ack)
	}
	c.logger().Info("tunnel established", "public_host", ack.PublicHost, "public_port", ack.PublicPort, "relay_id", ack.RelayID)

	session, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return time.Since(establishedAt), fmt.Errorf("start yamux client: %w", err)
	}
	defer session.Close()

	go func() {
		<-onceCtx.Done()
		_ = session.Close()
	}()

	typed := ack.StreamTyping
	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return time.Since(establishedAt), nil
			}
			return time.Since(establishedAt), fmt.Errorf("accept tunnel stream: %w", err)
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
	var dir punchcore.PunchDirective
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
	if c.Stats == nil {
		pipe(stream, target)
		return
	}
	c.Stats.active.Add(1)
	c.Stats.totalStreams.Add(1)
	defer c.Stats.active.Add(-1)
	countedPipe(stream, target, &c.Stats.toClients, &c.Stats.fromClients)
}

// countedPipe is pipe with per-direction byte accounting: bytes written to a
// are added to aBytes, bytes written to b to bBytes.
func countedPipe(a, b net.Conn, aBytes, bBytes *atomic.Uint64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(a, b)
		aBytes.Add(uint64(n))
		_ = a.Close()
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(b, a)
		bBytes.Add(uint64(n))
		_ = b.Close()
	}()
	wg.Wait()
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
