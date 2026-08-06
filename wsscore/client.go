package wsscore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

var ErrSocketProtectionFailed = errors.New("WSS socket protection failed")

// SocketProtector is deliberately gomobile-friendly. An Android adapter
// should implement Protect by calling VpnService.protect(fd), returning its
// boolean result. Protection happens before the CDN socket connects.
type SocketProtector interface {
	Protect(fd int32) bool
}

type ClientOptions struct {
	URL    string
	Ticket string

	TLSConfig       *tls.Config
	WebSocketDialer *websocket.Dialer

	// NativeFrontNoSNI omits ClientHello SNI for native one-label CDN URLs
	// whose zone default certificate covers them (*.cloudfront.net,
	// *.b-cdn.net) while retaining certificate verification against the signed
	// URL hostname. It has no effect on custom CNAMEs or other CDNs.
	NativeFrontNoSNI bool

	// CloudFrontNoSNI is the former name of NativeFrontNoSNI, from when
	// CloudFront was the only recognized zone. Either field enables the same
	// behavior, so pinned consumers keep building across the rename.
	//
	// Deprecated: use NativeFrontNoSNI.
	CloudFrontNoSNI bool

	HandshakeTimeout time.Duration
	PingInterval     time.Duration
	PingWriteTimeout time.Duration
	ReadLimit        int64
	LoopbackHost     string
	Lifecycle        LifecycleOptions
	SocketProtector  SocketProtector
}

// Client owns the WSS connection, yamux session, and loopback listener that a
// local Reality client dials. The opaque ticket is sent only in Authorization.
type Client struct {
	conn       *WebSocketConn
	session    *yamux.Session
	listener   net.Listener
	idle       *IdleGuard
	slots      chan struct{}
	streamIdle time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	serveMu   sync.Mutex
	serving   bool
	activeMu  sync.Mutex
	active    map[net.Conn]struct{}
	closeOnce sync.Once

	endMu sync.Mutex
	end   SessionEnd
}

func DialClient(ctx context.Context, opts ClientOptions) (*Client, error) {
	if err := ValidateFrontURL(opts.URL); err != nil {
		return nil, err
	}
	verificationName, nativeFront := nativeFrontHost(opts.URL)
	omitSNI := (opts.NativeFrontNoSNI || opts.CloudFrontNoSNI) && nativeFront
	if len(opts.Ticket) == 0 || len(opts.Ticket) > MaxTicketBytes || strings.ContainsAny(opts.Ticket, "\r\n") {
		return nil, errors.New("WSS ticket is missing or oversized")
	}
	handshakeTimeout, pingInterval, pingWriteTimeout, readLimit, lifecycle, err := normalizeClientOptions(opts)
	if err != nil {
		return nil, err
	}
	if opts.WebSocketDialer != nil && opts.WebSocketDialer.NetDialTLSContext != nil {
		return nil, errors.New("custom TLS dial callbacks are not allowed for WSS")
	}
	if opts.SocketProtector != nil && opts.WebSocketDialer != nil && opts.WebSocketDialer.NetDialContext != nil {
		return nil, errors.New("WSS socket protector cannot be combined with custom network dial callbacks")
	}
	tlsConfig, err := normalizedTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	if omitSNI && len(tlsConfig.EncryptedClientHelloConfigList) != 0 {
		return nil, errors.New("WSS native-front no-SNI mode cannot be combined with encrypted client hello")
	}

	loopback := opts.LoopbackHost
	if loopback == "" {
		loopback = "127.0.0.1"
	}
	ip := net.ParseIP(strings.Trim(loopback, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("WSS listener must bind a loopback IP literal")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		return nil, fmt.Errorf("listen for local WSS connections: %w", err)
	}
	keepListener := false
	defer func() {
		if !keepListener {
			_ = listener.Close()
		}
	}()

	dialer := websocket.Dialer{}
	if opts.WebSocketDialer != nil {
		dialer = *opts.WebSocketDialer
	}
	dialer.HandshakeTimeout = handshakeTimeout
	dialer.EnableCompression = false
	dialer.Subprotocols = []string{Subprotocol}
	// Never inherit environment or system proxies: the outer connection would
	// otherwise recurse once the local proxy points at the inner tunnel.
	dialer.Proxy = nil
	dialer.Jar = nil
	dialer.TLSClientConfig = tlsConfig
	phases := &dialPhases{}
	if dialer.NetDialContext == nil {
		networkDialer := newNetworkDialer(handshakeTimeout, opts.SocketProtector, phases)
		dialer.NetDialContext = networkDialer.DialContext
	}
	// Mark TCP completion on whichever network dial is in use (module default
	// or caller supplied) so timeout and reset tokens can name the failing
	// handshake phase in both TLS modes.
	netDial := dialer.NetDialContext
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := netDial(ctx, network, address)
		if err == nil {
			phases.tcpConnected.Store(true)
		}
		return conn, err
	}
	if omitSNI {
		// Gorilla otherwise fills an empty TLS ServerName from the URL host.
		// Complete TLS here so the distribution name remains confined to the
		// encrypted HTTP Host header while the protected/custom TCP dial path is
		// preserved.
		dialer.NetDialTLSContext = noSNITLSDialContext(dialer.NetDialContext, tlsConfig, verificationName, phases)
	}

	header := make(http.Header)
	header.Set(TicketAuthorizationHeader, TicketBearerPrefix+opts.Ticket)
	// Ordinary-SNI dials get their TLS phase marks from httptrace (gorilla
	// runs its own TLS only when NetDialTLSContext is unset); the no-SNI dial
	// marks the same booleans inside noSNITLSDialContext. The first-response
	// mark fires in both modes. GotConn is deliberately not registered — its
	// GotConnInfo.Conn exposes the resolved edge address. WithClientTrace
	// composes with any caller-installed trace.
	dialCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { phases.tlsStarted.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				phases.tlsDone.Store(true)
			}
		},
		GotFirstResponseByte: func() { phases.gotFirstResponseByte.Store(true) },
	})
	ws, resp, err := dialer.DialContext(dialCtx, opts.URL, header)
	if err != nil {
		// Classify while the typed error chain, the CDN status, and the phase
		// booleans all still exist, then destroy everything but the token.
		reason := classifyDialFailure(dialCtx, err, resp, phases)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, ErrSocketProtectionFailed) {
			return nil, ErrSocketProtectionFailed
		}
		return nil, newDialError(reason)
	}
	if resp != nil && len(resp.Header.Values("Sec-WebSocket-Extensions")) != 0 {
		_ = ws.Close()
		return nil, newDialErrorWithMessage(ReasonWSSubprotocol, "WSS extensions were unexpectedly negotiated")
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.Close()
		return nil, newDialErrorWithMessage(ReasonWSSubprotocol, "WSS subprotocol was not negotiated")
	}
	streamConn, err := NewWebSocketConn(ws, readLimit)
	if err != nil {
		_ = ws.Close()
		return nil, err
	}
	session, err := NewClientSession(streamConn)
	if err != nil {
		_ = streamConn.Close()
		return nil, errors.New("WSS multiplexer failed")
	}

	lifetimeCtx, cancel := context.WithTimeout(context.Background(), lifecycle.SessionLifetime)
	client := &Client{
		conn: streamConn, session: session, listener: listener,
		slots: make(chan struct{}, lifecycle.MaxConcurrentStreams), streamIdle: lifecycle.StreamIdleTimeout,
		ctx: lifetimeCtx, cancel: cancel, active: make(map[net.Conn]struct{}),
	}
	// The no-stream idle guard is opt-in for a client. A client session belongs
	// to the caller, which knows whether it still wants the transport; a VPN
	// deliberately holds an idle tunnel open while its user is not browsing, and
	// a guard armed at dial time would close that tunnel out from under it.
	// Sessions remain bounded by SessionLifetime and by the caller's own Close.
	if lifecycle.NoStreamIdleTimeout > 0 {
		client.idle, err = NewIdleGuard(lifecycle.NoStreamIdleTimeout, func() {
			client.closeWith(SessionEndIdle)
		})
		if err != nil {
			_ = client.Close()
			return nil, err
		}
	}
	if err := streamConn.StartPings(lifetimeCtx, pingInterval, pingWriteTimeout); err != nil {
		_ = client.Close()
		return nil, err
	}
	go func() {
		select {
		case <-lifetimeCtx.Done():
			client.closeWith(SessionEndLifetime)
		case <-session.CloseChan():
			// yamux tears the session down after the transport read fails, so the
			// peer's close code (if it sent one) is already recorded here.
			client.closeWith(sessionEndForCloseCode(streamConn.RemoteCloseCode()))
		}
	}()
	keepListener = true
	return client, nil
}

func normalizedTLSConfig(opts ClientOptions) (*tls.Config, error) {
	configured := opts.TLSConfig
	if configured == nil && opts.WebSocketDialer != nil {
		configured = opts.WebSocketDialer.TLSClientConfig
	}
	if configured == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	config := configured.Clone()
	if config.InsecureSkipVerify {
		return nil, errors.New("WSS TLS verification cannot be disabled")
	}
	if config.ServerName != "" {
		return nil, errors.New("WSS TLS server name must come from the signed front URL")
	}
	if config.MinVersion != 0 && config.MinVersion < tls.VersionTLS12 {
		return nil, errors.New("WSS TLS minimum version must be TLS 1.2 or newer")
	}
	if config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS12 {
		return nil, errors.New("WSS TLS maximum version must allow TLS 1.2 or newer")
	}
	if config.MinVersion == 0 {
		config.MinVersion = tls.VersionTLS12
	}
	return config, nil
}

func normalizeClientOptions(opts ClientOptions) (time.Duration, time.Duration, time.Duration, int64, LifecycleOptions, error) {
	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout == 0 {
		handshakeTimeout = DefaultHandshakeTimeout
	}
	if handshakeTimeout < time.Millisecond || handshakeTimeout > MaxHandshakeTimeout {
		return 0, 0, 0, 0, LifecycleOptions{}, fmt.Errorf("handshake timeout must be within [1ms, %s]", MaxHandshakeTimeout)
	}
	pingInterval := opts.PingInterval
	if pingInterval == 0 {
		pingInterval = DefaultPingInterval
	}
	if pingInterval < 0 {
		pingInterval = 0
	}
	if pingInterval > MaxPingInterval {
		return 0, 0, 0, 0, LifecycleOptions{}, fmt.Errorf("ping interval must not exceed %s", MaxPingInterval)
	}
	pingWriteTimeout := opts.PingWriteTimeout
	if pingWriteTimeout <= 0 {
		pingWriteTimeout = DefaultPingWriteTimeout
	}
	if pingWriteTimeout > MaxPingWriteTimeout || (pingInterval > 0 && pingWriteTimeout >= pingInterval) {
		return 0, 0, 0, 0, LifecycleOptions{}, fmt.Errorf("ping write timeout must be at most %s and shorter than the ping interval", MaxPingWriteTimeout)
	}
	readLimit := opts.ReadLimit
	if readLimit == 0 {
		readLimit = DefaultWebSocketReadMax
	}
	if readLimit < 1 || readLimit > MaxWebSocketReadMax {
		return 0, 0, 0, 0, LifecycleOptions{}, fmt.Errorf("WebSocket message read limit must be within [1, %d]", MaxWebSocketReadMax)
	}
	lifecycle, err := NormalizeLifecycleOptions(opts.Lifecycle)
	if err != nil {
		return 0, 0, 0, 0, LifecycleOptions{}, err
	}
	return handshakeTimeout, pingInterval, pingWriteTimeout, readLimit, lifecycle, nil
}

func socketControl(protector SocketProtector) func(context.Context, string, string, syscall.RawConn) error {
	return func(_ context.Context, _, _ string, raw syscall.RawConn) error {
		var protectErr error
		controlErr := raw.Control(func(fd uintptr) {
			const maxInt32 = uintptr(1<<31 - 1)
			if fd > maxInt32 || !protector.Protect(int32(fd)) {
				protectErr = ErrSocketProtectionFailed
			}
		})
		if controlErr != nil {
			return controlErr
		}
		return protectErr
	}
}

func newNetworkDialer(timeout time.Duration, protector SocketProtector, phases *dialPhases) *net.Dialer {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	var protectorControl func(context.Context, string, string, syscall.RawConn) error
	if protector != nil {
		protectorControl = socketControl(protector)
	}
	// The bogon observation always runs; protection semantics are delegated
	// unchanged (bogonAwareControl never vetoes or alters the dial).
	dialer.ControlContext = bogonAwareControl(phases, protectorControl)
	return dialer
}

// SessionEnd reports why the session stopped, or SessionEndNone while it is
// still running. Callers must consult it instead of inferring loss from Serve
// returning: Serve returns a nil error for every orderly end, including the
// client's own idle shutdown.
func (c *Client) SessionEnd() SessionEnd {
	if c == nil {
		return SessionEndNone
	}
	c.endMu.Lock()
	defer c.endMu.Unlock()
	return c.end
}

// RemoteCloseCode reports the WebSocket close code the peer sent, or zero when
// none was observed.
func (c *Client) RemoteCloseCode() int {
	if c == nil {
		return 0
	}
	return c.conn.RemoteCloseCode()
}

// closeWith records why the session ended and then closes it. The first reason
// recorded wins, so the cause that actually initiated teardown survives the
// cascade of closes that follows it.
func (c *Client) closeWith(reason SessionEnd) {
	if c == nil {
		return
	}
	c.markEnd(reason)
	_ = c.Close()
}

func (c *Client) markEnd(reason SessionEnd) {
	c.endMu.Lock()
	defer c.endMu.Unlock()
	if c.end == SessionEndNone {
		c.end = reason
	}
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
		return errors.New("WSS client is not initialized")
	}
	c.serveMu.Lock()
	if c.serving {
		c.serveMu.Unlock()
		return errors.New("WSS client Serve called more than once")
	}
	c.serving = true
	c.serveMu.Unlock()

	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.ctx.Done():
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
		select {
		case c.slots <- struct{}{}:
		default:
			_ = local.Close()
			continue
		}
		if c.idle != nil && !c.idle.Start() {
			<-c.slots
			_ = local.Close()
			continue
		}
		wg.Add(1)
		c.activeMu.Lock()
		c.active[local] = struct{}{}
		c.activeMu.Unlock()
		go func(local net.Conn) {
			defer wg.Done()
			defer c.idle.Done()
			defer func() { <-c.slots }()
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
			CopyOpaque(c.ctx, local, stream, c.streamIdle)
		}(local)
	}
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	c.closeOnce.Do(func() {
		// Every internal teardown path records its cause before reaching here, so
		// an unattributed close is the caller's own. Recording before the listener
		// closes keeps Serve's return ordered after the reason is known.
		c.markEnd(SessionEndLocal)
		if c.cancel != nil {
			c.cancel()
		}
		if c.idle != nil {
			c.idle.Close()
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
