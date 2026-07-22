package wssbridge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"openrung/internal/relay"
)

const (
	BridgePath                 = relay.WSSBridgePath
	HealthPath                 = "/healthz"
	OriginTokenHeader          = "X-OpenRung-Origin-Token"
	DefaultViewerAddressHeader = "CloudFront-Viewer-Address"

	defaultHandshakeTimeout = 10 * time.Second
	defaultPingInterval     = 30 * time.Second
	defaultPingWriteTimeout = 10 * time.Second
	defaultWebSocketReadMax = int64(1 << 20)
)

type ClientOptions struct {
	URL    string
	Ticket string

	TLSConfig        *tls.Config
	WebSocketDialer  *websocket.Dialer
	HandshakeTimeout time.Duration
	PingInterval     time.Duration
	PingWriteTimeout time.Duration
	ReadLimit        int64
	LoopbackHost     string
}

// Client owns the WSS connection, yamux session, and loopback listener that a
// local Reality client dials. The ticket is sent only in Authorization.
type Client struct {
	conn     *websocketStreamConn
	session  *yamux.Session
	listener net.Listener

	ctx    context.Context
	cancel context.CancelFunc

	serveMu   sync.Mutex
	serving   bool
	activeMu  sync.Mutex
	active    map[net.Conn]struct{}
	closeOnce sync.Once
}

func DialClient(ctx context.Context, opts ClientOptions) (*Client, error) {
	if err := validateBridgeURL(opts.URL); err != nil {
		return nil, err
	}
	if len(opts.Ticket) == 0 || len(opts.Ticket) > MaxTicketBytes || strings.ContainsAny(opts.Ticket, "\r\n") {
		return nil, errors.New("WSS bridge ticket is missing or oversized")
	}
	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	readLimit := opts.ReadLimit
	if readLimit <= 0 {
		readLimit = defaultWebSocketReadMax
	}
	pingInterval := opts.PingInterval
	if pingInterval == 0 {
		pingInterval = defaultPingInterval
	}
	if pingInterval < 0 {
		pingInterval = 0
	}
	pingWriteTimeout := opts.PingWriteTimeout
	if pingWriteTimeout <= 0 {
		pingWriteTimeout = defaultPingWriteTimeout
	}

	dialer := websocket.Dialer{
		HandshakeTimeout:  handshakeTimeout,
		EnableCompression: false,
		Subprotocols:      []string{Subprotocol},
		NetDialContext:    (&net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}).DialContext,
	}
	if opts.WebSocketDialer != nil {
		dialer = *opts.WebSocketDialer
		dialer.HandshakeTimeout = handshakeTimeout
		dialer.EnableCompression = false
		dialer.Subprotocols = []string{Subprotocol}
	}
	// Never inherit environment or system proxies: the outer connection would
	// otherwise recurse once the local proxy points at sing-box.
	dialer.Proxy = nil
	if opts.TLSConfig != nil {
		dialer.TLSClientConfig = opts.TLSConfig.Clone()
	}

	header := make(http.Header)
	header.Set("Authorization", "Bearer "+opts.Ticket)
	ws, resp, err := dialer.DialContext(ctx, opts.URL, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New("WSS bridge handshake failed")
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.Close()
		return nil, errors.New("WSS bridge subprotocol was not negotiated")
	}
	streamConn := newWebsocketStreamConn(ws, readLimit)
	session, err := yamux.Client(streamConn, bridgeYamuxConfig())
	if err != nil {
		_ = streamConn.Close()
		return nil, errors.New("WSS bridge multiplexer failed")
	}

	loopback := opts.LoopbackHost
	if loopback == "" {
		loopback = "127.0.0.1"
	}
	ip := net.ParseIP(strings.Trim(loopback, "[]"))
	if ip == nil || !ip.IsLoopback() {
		_ = session.Close()
		_ = streamConn.Close()
		return nil, errors.New("WSS bridge listener must bind a loopback IP literal")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		_ = session.Close()
		_ = streamConn.Close()
		return nil, fmt.Errorf("listen for local WSS bridge connections: %w", err)
	}

	lifetimeCtx, cancel := context.WithCancel(context.Background())
	client := &Client{
		conn: streamConn, session: session, listener: listener,
		ctx: lifetimeCtx, cancel: cancel, active: make(map[net.Conn]struct{}),
	}
	streamConn.startPings(lifetimeCtx, pingInterval, pingWriteTimeout)
	return client, nil
}

func (c *Client) Endpoint() (host string, port int) {
	if c == nil || c.listener == nil {
		return "", 0
	}
	addr, ok := c.listener.Addr().(*net.TCPAddr)
	if !ok {
		return "", 0
	}
	return addr.IP.String(), addr.Port
}

func (c *Client) Serve(ctx context.Context) error {
	if c == nil || c.listener == nil || c.session == nil {
		return errors.New("WSS bridge client is not initialized")
	}
	c.serveMu.Lock()
	if c.serving {
		c.serveMu.Unlock()
		return errors.New("WSS bridge client Serve called more than once")
	}
	c.serving = true
	c.serveMu.Unlock()

	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.ctx.Done():
		case <-c.session.CloseChan():
			_ = c.Close()
		case <-stopWatcher:
		}
	}()
	defer close(stopWatcher)

	var wg sync.WaitGroup
	for {
		local, err := c.listener.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil || c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		c.activeMu.Lock()
		c.active[local] = struct{}{}
		c.activeMu.Unlock()
		go func() {
			defer wg.Done()
			defer func() {
				c.activeMu.Lock()
				delete(c.active, local)
				c.activeMu.Unlock()
				_ = local.Close()
			}()
			stream, err := c.session.Open()
			if err != nil {
				return
			}
			defer stream.Close()
			copyOpaque(c.ctx, local, stream, 0)
		}()
	}
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.listener != nil {
			_ = c.listener.Close()
		}
		c.activeMu.Lock()
		for local := range c.active {
			_ = local.Close()
		}
		c.activeMu.Unlock()
		if c.session != nil {
			closeErr = c.session.Close()
		}
		if c.conn != nil {
			if err := c.conn.Close(); closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func validateBridgeURL(raw string) error {
	if strings.TrimSpace(raw) != raw {
		return errors.New("WSS bridge URL must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("WSS bridge URL must include scheme and host")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return errors.New("WSS bridge URL must not contain user info, query, or fragment")
	}
	if parsed.Path != BridgePath || parsed.RawPath != "" {
		return fmt.Errorf("WSS bridge URL path must be %s", BridgePath)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "wss":
		return nil
	case "ws":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("cleartext ws is allowed only on loopback for development")
	default:
		return errors.New("WSS bridge URL scheme must be wss")
	}
}
